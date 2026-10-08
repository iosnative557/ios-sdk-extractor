package extract

import (
	"io/fs"
	"os"
	"path/filepath"
)

type dirSource struct {
	root string
}

func (s *dirSource) walk(want func(dir string) bool, fn func(e *entry) error) error {
	return filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.Name() == ".DS_Store" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := &entry{path: rel, mode: info.Mode(), mtime: info.ModTime()}
		switch {
		case d.IsDir():
			if !want(rel) {
				return fs.SkipDir
			}
			return fn(e)
		case info.Mode()&fs.ModeSymlink != 0:
			if e.link, err = os.Readlink(p); err != nil {
				return err
			}
			return fn(e)
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			e.body = f
			return fn(e)
		}
		return nil
	})
}
