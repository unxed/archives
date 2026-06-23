package archives

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/unxed/sevenzip"
)

func init() {
	RegisterFormat(SevenZip{})

	// looks like the sevenzip package registers a lot of decompressors for us automatically:
	// https://github.com/bodgit/sevenzip/blob/46c5197162c784318b98b9a3f80289a9aa1ca51a/register.go#L38-L61
}

type SevenZip struct {
	// If true, errors encountered during reading or writing
	// a file within an archive will be logged and the
	// operation will continue on remaining files.
	ContinueOnError bool

	// The password, if dealing with an encrypted archive.
	Password string

	// If true, multiple files will be grouped into a single continuous stream
	// (solid archive), achieving higher compression ratios.
	Solid bool
}

func (SevenZip) Extension() string { return ".7z" }
func (SevenZip) MediaType() string { return "application/x-7z-compressed" }

func (z SevenZip) Match(_ context.Context, filename string, stream io.Reader) (MatchResult, error) {
	var mr MatchResult

	// match filename
	if filepath.Ext(strings.ToLower(filename)) == z.Extension() {
		mr.ByName = true
	}

	// match file header
	buf, err := readAtMost(stream, len(sevenZipHeader))
	if err != nil {
		return mr, err
	}
	mr.ByStream = bytes.Equal(buf, sevenZipHeader)

	return mr, nil
}

func (z SevenZip) Archive(ctx context.Context, output io.Writer, files []FileInfo) error {
	ws, ok := output.(io.WriteSeeker)
	if !ok {
		return fmt.Errorf("7z format requires an io.WriteSeeker to build the archive header")
	}

	szw, err := sevenzip.NewWriter(ws, sevenzip.WithSolid(z.Solid), sevenzip.WithPassword(z.Password))
	if err != nil {
		return err
	}
	defer szw.Close()

	for i, file := range files {
		if err := z.archiveOneFile(ctx, szw, i, file); err != nil {
			if z.ContinueOnError && ctx.Err() == nil {
				log.Printf("[ERROR] %v", err)
				continue
			}
			return err
		}
	}

	return nil
}

func (z SevenZip) archiveOneFile(ctx context.Context, szw *sevenzip.Writer, idx int, file FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err // honor context cancellation
	}

	fh := &sevenzip.FileHeader{
		Name:       file.NameInArchive,
		Modified:   file.ModTime(),
		Attributes: uint32(file.Mode()) << 16, // Map POSIX file modes to attributes
	}

	w, err := szw.CreateHeader(fh)
	if err != nil {
		return fmt.Errorf("creating header for file %d: %s: %w", idx, file.NameInArchive, err)
	}

	if file.IsDir() {
		return nil
	}

	if err := openAndCopyFile(file, w); err != nil {
		return fmt.Errorf("writing file %d: %s: %w", idx, file.NameInArchive, err)
	}

	return nil
}

func (z SevenZip) Extract(ctx context.Context, sourceArchive io.Reader, handleFile FileHandler) error {
	sra, ok := sourceArchive.(seekReaderAt)
	if !ok {
		return fmt.Errorf("input type must be an io.ReaderAt and io.Seeker because of zip format constraints")
	}

	size, err := streamSizeBySeeking(sra)
	if err != nil {
		return fmt.Errorf("determining stream size: %w", err)
	}

	zr, err := sevenzip.NewReaderWithPassword(sra, size, z.Password)
	if err != nil {
		return err
	}

	// important to initialize to non-nil, empty value due to how fileIsIncluded works
	skipDirs := skipList{}

	// Группируем файлы по независимым сжатым потокам (folders/streams).
	// Директории и пустые файлы (без потоков) обрабатываются отдельно.
	streamGroups := make(map[int][]*sevenzip.File)
	var independentFiles []*sevenzip.File

	for _, f := range zr.File {
		if fileIsIncluded(skipDirs, f.Name) {
			continue
		}
		if f.FileInfo().IsDir() || f.UncompressedSize == 0 {
			independentFiles = append(independentFiles, f)
		} else {
			streamGroups[f.Stream] = append(streamGroups[f.Stream], f)
		}
	}

	// 1. Синхронно создаем все директории на главном потоке, чтобы воркеры не писали в несуществующие пути.
	for _, f := range independentFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		fi := f.FileInfo()
		file := FileInfo{
			FileInfo:      fi,
			Header:        f.FileHeader,
			NameInArchive: f.Name,
			Open: func() (fs.File, error) {
				openedFile, err := f.Open()
				if err != nil {
					return nil, err
				}
				return fileInArchive{openedFile, fi}, nil
			},
		}

		err := handleFile(ctx, file)
		if errors.Is(err, fs.SkipAll) {
			return nil
		} else if errors.Is(err, fs.SkipDir) && file.IsDir() {
			skipDirs.add(f.Name)
		} else if err != nil {
			if z.ContinueOnError {
				log.Printf("[ERROR] %s: %v", f.Name, err)
				continue
			}
			return fmt.Errorf("handling independent file %s: %w", f.Name, err)
		}
	}

	if len(streamGroups) == 0 {
		return nil
	}

	// 2. Параллельно распаковываем независимые потоки сжатия через пул горутин.
	type streamJob struct {
		streamID int
		files    []*sevenzip.File
	}

	numWorkers := runtime.NumCPU()
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > len(streamGroups) {
		numWorkers = len(streamGroups)
	}

	jobCh := make(chan streamJob, len(streamGroups))
	for id, files := range streamGroups {
		jobCh <- streamJob{streamID: id, files: files}
	}
	close(jobCh)

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				for _, f := range job.files {
					if ctx.Err() != nil {
						return
					}
					fi := f.FileInfo()
					file := FileInfo{
						FileInfo:      fi,
						Header:        f.FileHeader,
						NameInArchive: f.Name,
						Open: func() (fs.File, error) {
							openedFile, err := f.Open()
							if err != nil {
								return nil, err
							}
							return fileInArchive{openedFile, fi}, nil
						},
					}
					// Последовательная распаковка файлов внутри одного потока сжатия
					if err := handleFile(ctx, file); err != nil {
						if errors.Is(err, fs.SkipAll) {
							return
						}
						select {
						case errCh <- fmt.Errorf("handling file %s: %w", f.Name, err):
						default:
						}
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)

	if err := <-errCh; err != nil {
		return err
	}

	return nil
}

// https://py7zr.readthedocs.io/en/latest/archive_format.html#signature
var sevenZipHeader = []byte("7z\xBC\xAF\x27\x1C")

// Interface guards
var (
	_ Archiver  = SevenZip{}
	_ Extractor = SevenZip{}
)
