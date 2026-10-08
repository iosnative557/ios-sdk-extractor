package extract

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

const LinkPatchFileName = ".LinkPatch.json"

type zipOut struct {
	path string
	f    *os.File
	zw   *zip.Writer
	err  error

	names  map[string]bool
	parent map[string]dirMeta
	links  map[string]string
	done   bool
}

type dirMeta struct {
	mode  fs.FileMode
	mtime time.Time
}

func newZipOut(path string) *zipOut {
	z := &zipOut{path: path, names: map[string]bool{}, parent: map[string]dirMeta{}, links: map[string]string{}}
	z.f, z.err = os.Create(path + ".tmp")
	if z.err == nil {
		z.zw = zip.NewWriter(z.f)
	}
	return z
}

func (z *zipOut) rememberDir(name string, e *entry) {
	if e.mode.IsDir() {
		z.parent[name] = dirMeta{e.mode, e.mtime}
	}
}

func (z *zipOut) ensureParents(name string, mtime time.Time) error {
	for i := 0; i < len(name); i++ {
		if name[i] != '/' {
			continue
		}
		dir := name[:i]
		if z.names[dir+"/"] {
			continue
		}
		m, ok := z.parent[dir]
		if !ok {
			m = dirMeta{0o755 | fs.ModeDir, mtime}
		}
		if err := z.addDir(dir, m.mode, m.mtime); err != nil {
			return err
		}
	}
	return nil
}

func (z *zipOut) dir(name string, mode fs.FileMode, mtime time.Time) error {
	if z.err != nil {
		return z.err
	}
	if z.names[name+"/"] {
		return nil
	}
	if err := z.ensureParents(name, mtime); err != nil {
		return err
	}
	return z.addDir(name, mode, mtime)
}

func (z *zipOut) addDir(name string, mode fs.FileMode, mtime time.Time) error {
	header := &zip.FileHeader{
		Name:     name + "/",
		Method:   zip.Store,
		Modified: mtime,
		Flags:    0x800,
	}

	header.SetMode(mode.Perm() | fs.ModeDir)
	if _, err := z.zw.CreateHeader(header); err != nil {
		return err
	}
	z.names[name+"/"] = true
	return nil
}

func (z *zipOut) file(name string, mode fs.FileMode, mtime time.Time, r io.Reader) error {
	if z.err != nil {
		return z.err
	}
	if z.names[name] {
		return fmt.Errorf("duplicate entry %s in %s", name, z.path)
	}
	if err := z.ensureParents(name, mtime); err != nil {
		return err
	}
	header := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: mtime,
		Flags:    0x800,
	}

	header.SetMode(mode.Perm())
	w, err := z.zw.CreateHeader(header)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	z.names[name] = true
	return nil
}

func (z *zipOut) link(name, target string, mtime time.Time) error {
	if z.err != nil {
		return z.err
	}
	if err := z.ensureParents(name, mtime); err != nil {
		return err
	}
	z.links[name] = target
	return nil
}

func (z *zipOut) finish() error {
	if z.err != nil {
		return z.err
	}
	linksJSON, err := json.MarshalIndent(z.links, "", "    ")
	if err != nil {
		return err
	}
	if err := z.writeRaw(LinkPatchFileName, linksJSON); err != nil {
		return err
	}
	if err := z.zw.Close(); err != nil {
		return err
	}
	if err := z.f.Close(); err != nil {
		return err
	}
	z.done = true
	return os.Rename(z.path+".tmp", z.path)
}

func (z *zipOut) writeRaw(name string, data []byte) error {
	w, err := z.zw.CreateHeader(&zip.FileHeader{Name: name, Flags: 0x800})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func (z *zipOut) abort() {
	if z.done || z.f == nil {
		return
	}
	z.done = true
	z.f.Close()
	os.Remove(z.path + ".tmp")
}
