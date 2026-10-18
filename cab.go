package archives

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"path"
	"path/filepath"
	"strings"
	"time"

	cabinet "github.com/abemedia/go-cabinet"
	"golang.org/x/text/encoding"
)

func init() {
	RegisterFormat(Cabinet{})
}

// Cabinet is the Microsoft Cabinet (.cab) archive format. It is read-only.
//
// Files stored uncompressed or with MS-ZIP are supported out of the box.
// Folders compressed with LZX or Quantum can be listed, but opening their
// files fails with cabinet.ErrAlgorithm unless a decompressor for the method
// is registered with cabinet.RegisterDecompressor. Cabinets that are split
// across several files (a "cabinet set") are not supported.
type Cabinet struct {
	// If true, errors encountered while handling a file within
	// an archive will be logged and the operation will continue
	// on remaining files.
	ContinueOnError bool

	// For files in cabinets whose names are not flagged as UTF-8
	// (most cabinets: the flag is rarely set by Windows tools),
	// specify the character encoding here, e.g. charmap.CodePage866.
	// Names that are plain ASCII are unaffected. If nil, names are
	// used as they are stored.
	TextEncoding encoding.Encoding
}

func (Cabinet) Extension() string { return ".cab" }
func (Cabinet) MediaType() string { return "application/vnd.ms-cab-compressed" }

func (c Cabinet) Match(_ context.Context, filename string, stream io.Reader) (MatchResult, error) {
	var mr MatchResult

	// match filename
	if filepath.Ext(strings.ToLower(filename)) == c.Extension() {
		mr.ByName = true
	}

	// match file header
	buf, err := readAtMost(stream, len(cabinetSignature))
	if err != nil {
		return mr, err
	}
	mr.ByStream = bytes.Equal(buf, cabinetSignature)

	return mr, nil
}

// Archive is not implemented for Cabinet: the format is read-only here.

// Extract walks the files of the cabinet in the order they are stored.
//
// sourceArchive must be an io.ReaderAt (an *os.File, *bytes.Reader and
// *io.SectionReader all are), because the cabinet directory and data blocks
// are read at arbitrary offsets. Otherwise an error is returned.
//
// Cabinets store the files of a folder as one compressed stream, so only one
// file of a given cabinet folder can be open at a time: close the file passed
// to handleFile before opening another one.
func (c Cabinet) Extract(ctx context.Context, sourceArchive io.Reader, handleFile FileHandler) error {
	ra, ok := sourceArchive.(io.ReaderAt)
	if !ok {
		return fmt.Errorf("input type must be an io.ReaderAt because of cabinet format constraints")
	}

	cr, err := cabinet.NewReader(ra)
	if err != nil {
		return err
	}

	// important to initialize to non-nil, empty value due to how fileIsIncluded works
	skipDirs := skipList{}

	for i, f := range cr.Files {
		if err := ctx.Err(); err != nil {
			return err // honor context cancellation
		}

		hdr := f.FileHeader
		c.decodeText(&hdr)

		if fileIsIncluded(skipDirs, hdr.Name) {
			continue
		}

		info := cabFileInfo{hdr: &hdr, size: int64(f.Size())}
		file := FileInfo{
			FileInfo:      info,
			Header:        hdr,
			NameInArchive: hdr.Name,
			Open: func() (fs.File, error) {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				return fileInArchive{rc, info}, nil
			},
		}

		err = handleFile(ctx, file)
		if errors.Is(err, fs.SkipAll) {
			break
		} else if errors.Is(err, fs.SkipDir) {
			// cabinets have no directory entries: skip the directory of the file
			dir := path.Dir(hdr.Name)
			if dir == "." {
				break
			}
			skipDirs.add(dir)
		} else if err != nil {
			if c.ContinueOnError {
				log.Printf("[ERROR] %s: %v", hdr.Name, err)
				continue
			}
			return fmt.Errorf("handling file %d: %s: %w", i, hdr.Name, err)
		}
	}

	return nil
}

// decodeText converts the file name to UTF-8. It is a no-op if the name
// is flagged as UTF-8 or if c.TextEncoding is not specified.
func (c Cabinet) decodeText(hdr *cabinet.FileHeader) {
	if hdr.NonUTF8 && c.TextEncoding != nil {
		if name, err := c.TextEncoding.NewDecoder().String(hdr.Name); err == nil {
			hdr.Name = name
		}
	}
}

// cabFileInfo satisfies the fs.FileInfo interface for cabinet entries.
type cabFileInfo struct {
	hdr  *cabinet.FileHeader
	size int64
}

func (ci cabFileInfo) Name() string { return path.Base(ci.hdr.Name) }
func (ci cabFileInfo) Size() int64  { return ci.size }
func (ci cabFileInfo) Mode() fs.FileMode {
	if ci.hdr.ReadOnly {
		return 0o444
	}
	return 0o644
}
func (ci cabFileInfo) ModTime() time.Time { return ci.hdr.Modified }
func (ci cabFileInfo) IsDir() bool        { return false }
func (ci cabFileInfo) Sys() any           { return ci.hdr }

// cabinetSignature is the CFHEADER signature ("MSCF") at the start of every cabinet.
var cabinetSignature = []byte("MSCF")

// Interface guard
var _ Extractor = Cabinet{}
