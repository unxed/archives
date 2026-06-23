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
	"strings"

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

// Extract extracts files from z, implementing the Extractor interface. Uniquely, however,
// sourceArchive must be an io.ReaderAt and io.Seeker, which are oddly disjoint interfaces
// from io.Reader which is what the method signature requires. We chose this signature for
// the interface because we figure you can Read() from anything you can ReadAt() or Seek()
// with. Due to the nature of the zip archive format, if sourceArchive is not an io.Seeker
// and io.ReaderAt, an error is returned.
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

	for i, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err // honor context cancellation
		}

		if fileIsIncluded(skipDirs, f.Name) {
			continue
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
			break
		} else if errors.Is(err, fs.SkipDir) && file.IsDir() {
			skipDirs.add(f.Name)
		} else if err != nil {
			if z.ContinueOnError {
				log.Printf("[ERROR] %s: %v", f.Name, err)
				continue
			}
			return fmt.Errorf("handling file %d: %s: %w", i, f.Name, err)
		}
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
