package archives

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	cabinet "github.com/abemedia/go-cabinet"
	"golang.org/x/text/encoding/charmap"
)

type cabTestFile struct {
	name string
	data []byte
	mod  time.Time
}

// buildCab writes a cabinet with the given files using go-cabinet's writer.
// Each group of files becomes its own cabinet folder with the given method.
func buildCab(t *testing.T, groups map[cabinet.Compression][]cabTestFile, order []cabinet.Compression) []byte {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "built.cab"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := cabinet.NewWriter(f)
	// a pass-through "compressor" lets us write LZX folders without an LZX
	// encoder; reading them back is what the tests need to be unsupported
	w.RegisterCompressor(cabinet.LZX, func(out io.Writer) (io.WriteCloser, error) {
		return nopWriteCloser{out}, nil
	})
	for _, method := range order {
		w.SetCompression(method)
		for _, file := range groups[method] {
			cw, err := w.CreateHeader(&cabinet.FileHeader{
				Name:     file.name,
				Modified: file.mod,
				NonUTF8:  !strings.ContainsRune(file.name, 'т'),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cw.Write(file.data); err != nil {
				t.Fatal(err)
			}
		}
		w.FlushFolder()
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func cabRandom(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

var cabTestMod = time.Date(2003, 2, 14, 10, 20, 0, 0, time.UTC)

// sampleCab has an uncompressed folder and an MS-ZIP folder, a subdirectory
// and a file (200000 bytes of noise) spanning several CFDATA blocks.
func sampleCab(t *testing.T) ([]byte, []cabTestFile) {
	t.Helper()
	files := []cabTestFile{
		{"plain.txt", []byte("hello, plain cabinet"), cabTestMod},
		{"sub/big.bin", cabRandom(1, 200000), cabTestMod.Add(2 * time.Hour)},
		{"zipped.txt", bytes.Repeat([]byte("zip me "), 5000), cabTestMod.Add(24 * time.Hour)},
		{"sub/deeper/z.txt", []byte("zipped, in a subdirectory"), cabTestMod},
	}
	data := buildCab(t, map[cabinet.Compression][]cabTestFile{
		cabinet.None:  files[:2],
		cabinet.MSZip: files[2:],
	}, []cabinet.Compression{cabinet.None, cabinet.MSZip})
	return data, files
}

func extractAllCab(t *testing.T, c Cabinet, r io.Reader) (names []string, contents map[string][]byte) {
	t.Helper()
	contents = map[string][]byte{}
	err := c.Extract(context.Background(), r, func(_ context.Context, info FileInfo) error {
		names = append(names, info.NameInArchive)
		f, err := info.Open()
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		contents[info.NameInArchive] = b
		return err
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return names, contents
}

func TestCabinetMatch(t *testing.T) {
	ctx := context.Background()
	data, _ := sampleCab(t)

	for _, tc := range []struct {
		name     string
		filename string
		stream   io.Reader
		want     MatchResult
	}{
		{"name and stream", "a.CAB", bytes.NewReader(data), MatchResult{ByName: true, ByStream: true}},
		{"stream only", "a.bin", bytes.NewReader(data), MatchResult{ByStream: true}},
		{"name only", "a.cab", strings.NewReader("not a cabinet"), MatchResult{ByName: true}},
		{"neither", "a.txt", strings.NewReader("MSC"), MatchResult{}},
		{"short stream", "", strings.NewReader("MS"), MatchResult{}},
		{"nil stream", "a.cab", nil, MatchResult{ByName: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Cabinet{}.Match(ctx, tc.filename, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}

	if _, err := (Cabinet{}).Match(ctx, "a.cab", iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Error("expected read error to be returned")
	}

	if ext, mt := (Cabinet{}).Extension(), (Cabinet{}).MediaType(); ext != ".cab" || mt != "application/vnd.ms-cab-compressed" {
		t.Errorf("unexpected extension/media type: %q %q", ext, mt)
	}
}

func TestCabinetIdentify(t *testing.T) {
	ctx := context.Background()
	data, _ := sampleCab(t)

	// by content only, from a seekable stream and from a plain reader
	for name, stream := range map[string]io.Reader{
		"seeker": bytes.NewReader(data),
		"reader": io.NopCloser(bytes.NewReader(data)),
	} {
		format, rewound, err := Identify(ctx, "installer.dat", stream)
		if err != nil {
			t.Fatalf("%s: Identify: %v", name, err)
		}
		if _, ok := format.(Cabinet); !ok {
			t.Fatalf("%s: got format %T, want Cabinet", name, format)
		}
		// the stream returned by Identify must still hold the whole file
		got, err := io.ReadAll(rewound)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: rewound stream differs from the input (err=%v)", name, err)
		}
	}

	// by name only
	format, _, err := Identify(ctx, "setup.cab", nil)
	if err != nil {
		t.Fatalf("Identify by name: %v", err)
	}
	if _, ok := format.(Cabinet); !ok {
		t.Errorf("got format %T, want Cabinet", format)
	}
}

func TestCabinetExtract(t *testing.T) {
	data, files := sampleCab(t)

	var wantNames []string
	want := map[string][]byte{}
	for _, f := range files {
		wantNames = append(wantNames, f.name)
		want[f.name] = f.data
	}

	// both readers that provide ReadAt must work
	tmp := filepath.Join(t.TempDir(), "sample.cab")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	osFile, err := os.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer osFile.Close()

	for name, r := range map[string]io.Reader{
		"bytes.Reader":  bytes.NewReader(data),
		"os.File":       osFile,
		"SectionReader": io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data))),
	} {
		t.Run(name, func(t *testing.T) {
			names, contents := extractAllCab(t, Cabinet{}, r)
			if !reflect.DeepEqual(names, wantNames) {
				t.Errorf("names: got %v, want %v", names, wantNames)
			}
			for n, w := range want {
				if !bytes.Equal(contents[n], w) {
					t.Errorf("%s: content mismatch (got %d bytes, want %d)", n, len(contents[n]), len(w))
				}
			}
		})
	}
}

func TestCabinetFileInfo(t *testing.T) {
	data, files := sampleCab(t)

	i := 0
	err := Cabinet{}.Extract(context.Background(), bytes.NewReader(data), func(_ context.Context, info FileInfo) error {
		want := files[i]
		i++
		if info.NameInArchive != want.name {
			t.Errorf("NameInArchive: got %q, want %q", info.NameInArchive, want.name)
		}
		if info.Name() != filepath.Base(want.name) {
			t.Errorf("Name: got %q, want %q", info.Name(), filepath.Base(want.name))
		}
		if info.Size() != int64(len(want.data)) {
			t.Errorf("%s: Size: got %d, want %d", want.name, info.Size(), len(want.data))
		}
		if info.IsDir() || info.Mode() != 0o644 {
			t.Errorf("%s: IsDir=%v Mode=%v", want.name, info.IsDir(), info.Mode())
		}
		if !info.ModTime().Equal(want.mod) {
			t.Errorf("%s: ModTime: got %v, want %v", want.name, info.ModTime(), want.mod)
		}
		if _, ok := info.Header.(cabinet.FileHeader); !ok {
			t.Errorf("%s: Header is %T, want cabinet.FileHeader", want.name, info.Header)
		}
		if _, ok := info.Sys().(*cabinet.FileHeader); !ok {
			t.Errorf("%s: Sys() is %T, want *cabinet.FileHeader", want.name, info.Sys())
		}

		f, err := info.Open()
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.Size() != info.Size() {
			t.Errorf("%s: Stat of opened file: %v, %v", want.name, st, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if i != len(files) {
		t.Errorf("handler called %d times, want %d", i, len(files))
	}
}

func TestCabinetReadOnlyMode(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "ro.cab"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := cabinet.NewWriter(f)
	cw, err := w.CreateHeader(&cabinet.FileHeader{Name: "ro.txt", Modified: cabTestMod, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cw.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	var mode fs.FileMode
	err = Cabinet{}.Extract(context.Background(), f, func(_ context.Context, info FileInfo) error {
		mode = info.Mode()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode != 0o444 {
		t.Errorf("got mode %v, want 0444", mode)
	}
}

// TestCabinetExtractMakecab reads a cabinet produced by Microsoft's makecab.exe
// (one MS-ZIP folder and one uncompressed folder), not by go-cabinet's own writer.
func TestCabinetExtractMakecab(t *testing.T) {
	f, err := os.Open("testdata/mixed_compression.cab")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	format, _, err := Identify(context.Background(), "mixed_compression.cab", f)
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := format.(Extractor)
	if !ok {
		t.Fatalf("%T is not an Extractor", format)
	}

	want := map[string]struct {
		size   int
		sha256 string
	}{
		"compressed.txt": {256, "925b08e69ad682eaae8ddc6c6e048372d8c66a01006c6b04dbc35e200793a728"},
		"plain.txt":      {128, "d52969473f969db7657157d0275812ad01785bfe9ca73c419d0274fd7ca59cf2"},
	}
	got := 0
	err = ex.Extract(context.Background(), f, func(_ context.Context, info FileInfo) error {
		w, ok := want[info.NameInArchive]
		if !ok {
			t.Errorf("unexpected file %q", info.NameInArchive)
			return nil
		}
		got++
		rc, err := info.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != w.sha256 || info.Size() != int64(len(b)) || len(b) != w.size {
			t.Errorf("%s: got %d bytes, sha256 %x", info.NameInArchive, len(b), sum)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != len(want) {
		t.Errorf("extracted %d files, want %d", got, len(want))
	}
}

func TestCabinetFileSystem(t *testing.T) {
	data, files := sampleCab(t)
	cabPath := filepath.Join(t.TempDir(), "fs.cab")
	if err := os.WriteFile(cabPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// once from a path, once from an open stream
	f, err := os.Open(cabPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for name, open := range map[string]func() (fs.FS, error){
		"path":   func() (fs.FS, error) { return FileSystem(context.Background(), cabPath, nil) },
		"stream": func() (fs.FS, error) { return FileSystem(context.Background(), cabPath, f) },
	} {
		t.Run(name, func(t *testing.T) {
			fsys, err := open()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := fsys.(*ArchiveFS); !ok {
				t.Fatalf("got %T, want *ArchiveFS", fsys)
			}

			entries, err := fs.ReadDir(fsys, ".")
			if err != nil {
				t.Fatal(err)
			}
			var top []string
			for _, e := range entries {
				top = append(top, e.Name())
			}
			if want := []string{"plain.txt", "sub", "zipped.txt"}; !reflect.DeepEqual(top, want) {
				t.Errorf("root listing: got %v, want %v", top, want)
			}

			for _, file := range files {
				got, err := fs.ReadFile(fsys, file.name)
				if err != nil {
					t.Errorf("ReadFile(%s): %v", file.name, err)
					continue
				}
				if !bytes.Equal(got, file.data) {
					t.Errorf("ReadFile(%s): content mismatch", file.name)
				}
			}

			var walked []string
			err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					walked = append(walked, p)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"plain.txt", "sub/big.bin", "sub/deeper/z.txt", "zipped.txt"}; !reflect.DeepEqual(walked, want) {
				t.Errorf("walk: got %v, want %v", walked, want)
			}
		})
	}
}

func TestCabinetExtractErrors(t *testing.T) {
	data, _ := sampleCab(t)
	ctx := context.Background()
	never := func(context.Context, FileInfo) error {
		t.Error("handler must not be called")
		return nil
	}

	// a stream without ReadAt cannot be used
	if err := (Cabinet{}).Extract(ctx, bytes.NewBuffer(data), never); err == nil || !strings.Contains(err.Error(), "io.ReaderAt") {
		t.Errorf("non-ReaderAt input: got %v", err)
	}
	if err := (Cabinet{}).Extract(ctx, nil, never); err == nil {
		t.Error("nil input: expected an error")
	}

	// not a cabinet at all, and a truncated one
	if err := (Cabinet{}).Extract(ctx, strings.NewReader("this is definitely not a cabinet file"), never); err == nil {
		t.Error("garbage input: expected an error")
	}
	if err := (Cabinet{}).Extract(ctx, bytes.NewReader(data[:20]), never); err == nil {
		t.Error("truncated input: expected an error")
	}

	// cancelled context is honoured
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := (Cabinet{}).Extract(cctx, bytes.NewReader(data), never); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: got %v", err)
	}
}

func TestCabinetHandlerResults(t *testing.T) {
	files := []cabTestFile{
		{"a/1.txt", []byte("1"), cabTestMod},
		{"a/2.txt", []byte("2"), cabTestMod},
		{"b/3.txt", []byte("3"), cabTestMod},
		{"4.txt", []byte("4"), cabTestMod},
		{"5.txt", []byte("5"), cabTestMod},
	}
	data := buildCab(t, map[cabinet.Compression][]cabTestFile{cabinet.None: files}, []cabinet.Compression{cabinet.None})
	ctx := context.Background()

	visit := func(c Cabinet, stopAt string, result error) ([]string, error) {
		var seen []string
		err := c.Extract(ctx, bytes.NewReader(data), func(_ context.Context, info FileInfo) error {
			seen = append(seen, info.NameInArchive)
			if info.NameInArchive == stopAt {
				return result
			}
			return nil
		})
		return seen, err
	}

	t.Run("SkipAll", func(t *testing.T) {
		seen, err := visit(Cabinet{}, "a/2.txt", fs.SkipAll)
		if err != nil || !reflect.DeepEqual(seen, []string{"a/1.txt", "a/2.txt"}) {
			t.Errorf("got %v, %v", seen, err)
		}
	})

	t.Run("SkipDir skips the directory of the file", func(t *testing.T) {
		seen, err := visit(Cabinet{}, "a/1.txt", fs.SkipDir)
		if err != nil || !reflect.DeepEqual(seen, []string{"a/1.txt", "b/3.txt", "4.txt", "5.txt"}) {
			t.Errorf("got %v, %v", seen, err)
		}
	})

	t.Run("SkipDir at the root stops the walk", func(t *testing.T) {
		seen, err := visit(Cabinet{}, "4.txt", fs.SkipDir)
		if err != nil || !reflect.DeepEqual(seen, []string{"a/1.txt", "a/2.txt", "b/3.txt", "4.txt"}) {
			t.Errorf("got %v, %v", seen, err)
		}
	})

	boom := errors.New("boom")

	t.Run("error is returned", func(t *testing.T) {
		seen, err := visit(Cabinet{}, "a/2.txt", boom)
		if !errors.Is(err, boom) || len(seen) != 2 {
			t.Errorf("got %v, %v", seen, err)
		}
	})

	t.Run("ContinueOnError", func(t *testing.T) {
		seen, err := visit(Cabinet{ContinueOnError: true}, "a/2.txt", boom)
		if err != nil || len(seen) != len(files) {
			t.Errorf("got %v, %v", seen, err)
		}
	})
}

func TestCabinetTextEncoding(t *testing.T) {
	const name = "документы/тест.txt"
	// DOS (OEM) code page names, as stored by Russian versions of Windows
	stored, err := charmap.CodePage866.NewEncoder().String(name)
	if err != nil {
		t.Fatal(err)
	}
	files := []cabTestFile{{stored, []byte("содержимое"), cabTestMod}}
	data := buildCab(t, map[cabinet.Compression][]cabTestFile{cabinet.None: files}, []cabinet.Compression{cabinet.None})

	first := func(c Cabinet) string {
		var got string
		err := c.Extract(context.Background(), bytes.NewReader(data), func(_ context.Context, info FileInfo) error {
			got = info.NameInArchive
			if info.Name() != path.Base(got) {
				t.Errorf("Name() %q does not match %q", info.Name(), got)
			}
			return fs.SkipAll
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := first(Cabinet{}); got != stored {
		t.Errorf("without TextEncoding the stored name must be kept: got %q", got)
	}
	if got := first(Cabinet{TextEncoding: charmap.CodePage866}); got != name {
		t.Errorf("with CP866: got %q, want %q", got, name)
	}

	// a name flagged as UTF-8 is never re-decoded
	utf8Files := []cabTestFile{{"тест.txt", []byte("x"), cabTestMod}}
	utf8Data := buildCab(t, map[cabinet.Compression][]cabTestFile{cabinet.None: utf8Files}, []cabinet.Compression{cabinet.None})
	var got string
	err = Cabinet{TextEncoding: charmap.CodePage866}.Extract(context.Background(), bytes.NewReader(utf8Data), func(_ context.Context, info FileInfo) error {
		got = info.NameInArchive
		return nil
	})
	if err != nil || got != "тест.txt" {
		t.Errorf("UTF-8 name: got %q, %v", got, err)
	}
}

func TestCabinetUnsupportedMethod(t *testing.T) {
	files := []cabTestFile{
		{"lzx.txt", []byte("pretend this is LZX"), cabTestMod},
	}
	plain := []cabTestFile{{"plain.txt", []byte("plain"), cabTestMod}}
	data := buildCab(t, map[cabinet.Compression][]cabTestFile{
		cabinet.LZX:  files,
		cabinet.None: plain,
	}, []cabinet.Compression{cabinet.LZX, cabinet.None})

	opened := map[string]error{}
	err := Cabinet{}.Extract(context.Background(), bytes.NewReader(data), func(_ context.Context, info FileInfo) error {
		// listing works for every folder; opening needs a decompressor
		f, err := info.Open()
		if err == nil {
			_ = f.Close()
		}
		opened[info.NameInArchive] = err
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(opened["lzx.txt"], cabinet.ErrAlgorithm) {
		t.Errorf("lzx.txt: got %v, want cabinet.ErrAlgorithm", opened["lzx.txt"])
	}
	if opened["plain.txt"] != nil {
		t.Errorf("plain.txt: %v", opened["plain.txt"])
	}
}
