package extract

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	log "log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

type xipSource struct {
	path string
}

func (s *xipSource) walk(want func(dir string) bool, fn func(e *entry) error) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	content, err := xarFile(f, "Content")
	if err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	progress := &progressReader{r: content, total: content.Size(), next: 0.1}
	stream := newPBZXReader(bufio.NewReaderSize(progress, 1<<20), runtime.GOMAXPROCS(0))
	defer stream.Close()

	spool, err := os.MkdirTemp("", "ios-sdk-extractor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(spool)
	w := &cpioWalker{fn: fn, spool: spool, links: map[inodeKey]*hardlink{}}
	if err := w.walk(bufio.NewReaderSize(stream, 1<<20)); err != nil {
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if w.app == "" {
		return fmt.Errorf("%s: no .app inside this xip", s.path)
	}
	return nil
}

type xarTOC struct {
	Files []xarEntry `xml:"toc>file"`
}

type xarEntry struct {
	Name string `xml:"name"`
	Data struct {
		Offset   int64 `xml:"offset"`
		Length   int64 `xml:"length"`
		Encoding struct {
			Style string `xml:"style,attr"`
		} `xml:"encoding"`
	} `xml:"data"`
}

func xarFile(r io.ReaderAt, name string) (*io.SectionReader, error) {
	var hdr struct {
		Magic            [4]byte
		HeaderSize       uint16
		Version          uint16
		TOCCompressed    uint64
		TOCUncompressed  uint64
		ChecksumAlgorism uint32
	}
	if err := binary.Read(io.NewSectionReader(r, 0, 28), binary.BigEndian, &hdr); err != nil {
		return nil, fmt.Errorf("not a xip (xar) file: %w", err)
	}
	if string(hdr.Magic[:]) != "xar!" {
		return nil, errors.New("not a xip (xar) file")
	}
	zr, err := zlib.NewReader(io.NewSectionReader(r, int64(hdr.HeaderSize), int64(hdr.TOCCompressed)))
	if err != nil {
		return nil, fmt.Errorf("xar toc: %w", err)
	}
	defer zr.Close()
	var toc xarTOC
	if err := xml.NewDecoder(zr).Decode(&toc); err != nil {
		return nil, fmt.Errorf("xar toc: %w", err)
	}
	heap := int64(hdr.HeaderSize) + int64(hdr.TOCCompressed)
	for _, f := range toc.Files {
		if f.Name != name {
			continue
		}
		if style := f.Data.Encoding.Style; style != "" && style != "application/octet-stream" {
			return nil, fmt.Errorf("xar entry %s is encoded as %s; expected application/octet-stream", name, style)
		}
		return io.NewSectionReader(r, heap+f.Data.Offset, f.Data.Length), nil
	}
	return nil, fmt.Errorf("no %s in this xip", name)
}

type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	next  float64
	start time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	if p.start.IsZero() {
		p.start = time.Now()
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.total > 0 && float64(p.n)/float64(p.total) >= p.next {
		log.Info("reading xip", "progress", fmt.Sprintf("%.0f%%", p.next*100), "elapsed", time.Since(p.start).Round(time.Second))
		p.next += 0.1
	}
	return n, err
}

const pbzxMoreChunks = 0x01000000

var xzMagic = []byte{0xfd, '7', 'z', 'X', 'Z', 0}

func newPBZXReader(r io.Reader, workers int) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(decodePBZX(r, pw, max(workers, 1))) }()
	return pr
}

type pbzxResult struct {
	data []byte
	err  error
}

func decodePBZX(r io.Reader, w io.Writer, workers int) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("pbzx: %w", err)
	}
	if string(magic[:]) != "pbzx" {
		return fmt.Errorf("xip Content is not a pbzx stream (magic %q)", magic[:])
	}
	var flags uint64
	if err := binary.Read(r, binary.BigEndian, &flags); err != nil {
		return fmt.Errorf("pbzx: %w", err)
	}

	type job struct {
		data []byte
		out  chan pbzxResult
	}
	jobs := make(chan job)
	order := make(chan chan pbzxResult, workers+2)
	stop := make(chan struct{})
	writeErr := make(chan error, 1)
	for range workers {
		go func() {
			for j := range jobs {
				j.out <- decodeChunk(j.data)
			}
		}()
	}
	go func() {
		var err error
		for ch := range order {
			res := <-ch
			if err == nil {
				err = res.err
				if err == nil {
					_, err = w.Write(res.data)
				}
				if err != nil {
					close(stop)
				}
			}
		}
		writeErr <- err
	}()

	readErr := func() error {
		defer close(jobs)
		for flags&pbzxMoreChunks != 0 {
			var head [2]uint64
			if err := binary.Read(r, binary.BigEndian, &head); err != nil {
				return fmt.Errorf("pbzx chunk header: %w", err)
			}
			flags = head[0]
			data := make([]byte, head[1])
			if _, err := io.ReadFull(r, data); err != nil {
				return fmt.Errorf("pbzx chunk: %w", err)
			}
			ch := make(chan pbzxResult, 1)
			select {
			case order <- ch:
			case <-stop:
				return nil
			}
			jobs <- job{data, ch}
		}
		return nil
	}()
	close(order)
	if err := <-writeErr; err != nil {
		return err
	}
	return readErr
}

func decodeChunk(data []byte) pbzxResult {
	if !bytes.HasPrefix(data, xzMagic) {
		return pbzxResult{data: data}
	}
	zr, err := xz.NewReader(bytes.NewReader(data))
	if err != nil {
		return pbzxResult{err: fmt.Errorf("pbzx xz chunk: %w", err)}
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return pbzxResult{err: fmt.Errorf("pbzx xz chunk: %w", err)}
	}
	return pbzxResult{data: out}
}

const (
	cpioHeaderSize = 76
	cpioTrailer    = "TRAILER!!!"
)

type cpioHeader struct {
	dev, ino, mode, nlink uint64
	mtime                 int64
	name                  string
	size                  int64
}

func octal(b []byte) (uint64, error) {
	return strconv.ParseUint(string(b), 8, 64)
}

func readCpioHeader(r io.Reader) (*cpioHeader, error) {
	var raw [cpioHeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return nil, fmt.Errorf("cpio header: %w", err)
	}
	if string(raw[:6]) != "070707" {
		return nil, fmt.Errorf("unsupported cpio format (magic %q, expected odc 070707)", raw[:6])
	}

	fields := []struct{ off, n int }{{6, 6}, {12, 6}, {18, 6}, {36, 6}, {48, 11}, {59, 6}, {65, 11}}
	var v [7]uint64
	for i, f := range fields {
		x, err := octal(raw[f.off : f.off+f.n])
		if err != nil {
			return nil, fmt.Errorf("cpio header field: %w", err)
		}
		v[i] = x
	}
	name := make([]byte, v[5])
	if _, err := io.ReadFull(r, name); err != nil {
		return nil, fmt.Errorf("cpio name: %w", err)
	}
	return &cpioHeader{
		dev: v[0], ino: v[1], mode: v[2], nlink: v[3], mtime: int64(v[4]),
		name: strings.TrimRight(string(name), "\x00"), size: int64(v[6]),
	}, nil
}

func (h *cpioHeader) fileMode() fs.FileMode {
	m := fs.FileMode(h.mode & 0o777)
	switch h.mode & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	case 0o100000:
	default:
		m |= fs.ModeIrregular
	}
	return m
}

type inodeKey struct{ dev, ino uint64 }

type hardlink struct {
	spooled   string
	remaining uint64
}

type cpioWalker struct {
	fn     func(e *entry) error
	want   func(dir string) bool
	spool  string
	links  map[inodeKey]*hardlink
	app    string
	nSpool int
}

func (w *cpioWalker) relPath(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")
	first, rest, _ := strings.Cut(name, "/")
	if !strings.HasSuffix(first, ".app") {
		return "", false
	}
	if w.app == "" {
		w.app = first
		log.Info("found app in xip", "app", first)
	} else if first != w.app {
		return "", false
	}
	return rest, rest != ""
}

func (w *cpioWalker) walk(r io.Reader) error {
	for {
		h, err := readCpioHeader(r)
		if err != nil {
			return err
		}
		if h.name == cpioTrailer {
			break
		}
		body := io.LimitReader(r, h.size)
		if err := w.entry(h, body); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, body); err != nil {
			return fmt.Errorf("cpio %s: %w", h.name, err)
		}
	}
	return nil
}

func (w *cpioWalker) entry(h *cpioHeader, body io.Reader) error {
	rel, ok := w.relPath(h.name)
	if !ok {
		return nil
	}
	e := &entry{path: rel, mode: h.fileMode(), mtime: time.Unix(h.mtime, 0)}
	switch {
	case e.mode.IsDir():
		return w.fn(e)
	case e.mode&fs.ModeSymlink != 0:
		target, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		e.link = string(target)
		return w.fn(e)
	case !e.mode.IsRegular():
		return nil
	case h.nlink <= 1:
		e.body = body
		return w.fn(e)
	}
	return w.hardlinked(h, e, body)
}

func (w *cpioWalker) hardlinked(h *cpioHeader, e *entry, body io.Reader) error {
	key := inodeKey{h.dev, h.ino}
	hl := w.links[key]
	if hl != nil && hl.remaining > 0 {
		hl.remaining--
		if hl.remaining == 0 {
			delete(w.links, key)
			defer os.Remove(hl.spooled)
		}
		return w.emitSpooled(e, hl.spooled)
	}

	w.nSpool++
	spooled := fmt.Sprintf("%s/%d", w.spool, w.nSpool)
	f, err := os.Create(spooled)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	w.links[key] = &hardlink{spooled: spooled, remaining: h.nlink - 1}
	return w.emitSpooled(e, spooled)
}

func (w *cpioWalker) emitSpooled(e *entry, spooled string) error {
	f, err := os.Open(spooled)
	if err != nil {
		return err
	}
	defer f.Close()
	e.body = f
	return w.fn(e)
}
