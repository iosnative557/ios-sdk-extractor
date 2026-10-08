package extract

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	log "log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"howett.net/plist"
)

const XcodeInfoDirName = "XcodeInfo"

const (
	toolchainRoot     = "Contents/Developer/Toolchains/XcodeDefault.xctoolchain"
	platformsRoot     = "Contents/Developer/Platforms"
	xcodeVersionPlist = "Contents/version.plist"

	hostSystemVersionPlist = "/System/Library/CoreServices/SystemVersion.plist"
)

//go:embed SystemVersion.plist
var defaultSystemVersion []byte

var swiftlangStampRe = regexp.MustCompile(`swiftlang-(\d+\.\d+\.\d+)`)

var appleSwiftVersionRe = regexp.MustCompile(`Apple Swift version (\d+(?:\.\d+)*)`)

const stampHeadSize = 1024

type Options struct {
	Platform string
	Output   string
	Xcode    string
}

type Product struct {
	Zip     string
	Name    string
	Version string
	Swift   string
}

func Run(o Options) ([]Product, error) {
	platforms := platformStringToPlatformNames(o.Platform)
	if len(platforms) == 0 {
		return nil, fmt.Errorf("no known platform in %q (iphone / watch / appletv / xros / mac)", o.Platform)
	}
	src, err := openSource(o.Xcode)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Output, 0o755); err != nil {
		return nil, err
	}
	p := newPacker(o.Output, platforms)
	defer p.abortAll()
	log.Info("extracting sdk and toolchain", "source", o.Xcode)
	if err := src.walk(p.want, p.handle); err != nil {
		return nil, err
	}
	return p.finish()
}

type source interface {
	walk(want func(dir string) bool, fn func(e *entry) error) error
}

type entry struct {
	path  string
	mode  fs.FileMode
	mtime time.Time
	link  string
	body  io.Reader
}

func openSource(xcode string) (source, error) {
	st, err := os.Stat(xcode)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return &xipSource{path: xcode}, nil
	}
	for _, dir := range []string{toolchainRoot, platformsRoot} {
		if st, err := os.Stat(filepath.Join(xcode, filepath.FromSlash(dir))); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("not an Xcode.app (missing %s): %s", dir, xcode)
		}
	}
	return &dirSource{root: xcode}, nil
}

func platformStringToPlatformNames(userInputPlatformString string) []string {
	var names []string
	for _, s := range strings.Split(userInputPlatformString, " ") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || strings.Contains(s, "simulator") {
			continue
		}
		name := ""
		switch {
		case strings.Contains(s, "iphone"):
			name = "iPhoneOS"
		case strings.Contains(s, "watch"):
			name = "WatchOS"
		case strings.Contains(s, "appletv"):
			name = "AppleTVOS"
		case strings.Contains(s, "xros"):
			name = "XROS"
		case strings.Contains(s, "mac"):
			name = "MacOSX"
		}
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

type target struct {
	name     string
	root     string
	includes []string
	exclude  *regexp.Regexp
	out      *zipOut

	stampDirs   []string
	marketingOK bool
	swift       string
	infoPlist   string
	info        []byte
	badNames    []string
}

func (t *target) match(e *entry) (string, bool) {
	r, ok := strings.CutPrefix(e.path, t.root+"/")
	if !ok {
		return "", false
	}
	if t.excluded(r) {
		return "", false
	}
	for _, inc := range t.includes {
		if r == inc || strings.HasPrefix(r, inc+"/") {
			return r, true
		}
	}
	return "", false
}

func (t *target) excluded(r string) bool {
	return t.exclude != nil && t.exclude.MatchString(r)
}

func (t *target) want(dir string) bool {
	if r, ok := strings.CutPrefix(dir, t.root+"/"); ok && t.excluded(r) {
		return false
	}
	for _, inc := range t.includes {
		f := t.root + "/" + inc
		if dir == f || strings.HasPrefix(dir, f+"/") || strings.HasPrefix(f, dir+"/") {
			return true
		}
	}
	return false
}

func (t *target) wantsStamp(e *entry) bool {
	if t.swift != "" || !e.mode.IsRegular() || !strings.HasSuffix(e.path, ".swiftinterface") {
		return false
	}
	r, ok := strings.CutPrefix(e.path, t.root+"/")
	if !ok {
		return false
	}
	for _, d := range t.stampDirs {
		if ok, _ := path.Match(d, dirPrefix(r, strings.Count(d, "/")+1)); ok {
			return true
		}
	}
	return false
}

func dirPrefix(r string, n int) string {
	parts := strings.SplitN(r, "/", n+1)
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[:n], "/")
}

func (t *target) takeStamp(head []byte) {
	if m := swiftlangStampRe.FindSubmatch(head); m != nil {
		t.swift = string(m[1])
	} else if t.marketingOK {
		if m := appleSwiftVersionRe.FindSubmatch(head); m != nil {
			t.swift = string(m[1])
		}
	}
}

var toolchainSwiftDirs = map[string]string{
	"iPhoneOS": "iphoneos", "WatchOS": "watchos", "AppleTVOS": "appletvos", "XROS": "xros", "MacOSX": "macosx",
}

func toolchainTarget(platforms []string) *target {
	includes := []string{"usr/lib/swift/_InternalSwiftScan", "usr/lib/swift/swiftToCxx", "usr/lib/swift_static/clang", "usr/lib/clang"}
	dirs := []string{"iphoneos"}
	for _, p := range platforms {
		if d := toolchainSwiftDirs[p]; d != "" && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		includes = append(includes,
			"usr/lib/swift/"+d,
			"usr/lib/swift-5.0/"+d+"/libswiftXCTest.dylib", "usr/lib/swift-5.5/"+d, "usr/lib/swift-6.2/"+d,
		)
	}
	return &target{
		name:     "toolchain",
		root:     toolchainRoot,
		includes: includes,
		exclude: regexp.MustCompile(`^usr/lib/clang/[^/]+/lib/darwin/libclang_rt\.[^/]*((ios|watchos|tvos|xros)sim|cc_kext|sepos|driverkit)` +
			`|^usr/lib/swift/[^/]+/prebuilt-modules(/|$)`),
		stampDirs:   []string{"usr/lib/swift/iphoneos", "usr/lib/swift-*/iphoneos"},
		marketingOK: true,
	}
}

func platformTarget(p string) *target {
	dir := p + ".platform"
	return &target{
		name: p,
		root: platformsRoot,
		includes: []string{
			dir + "/Info.plist",
			dir + "/version.plist",
			dir + "/Developer/SDKs/" + p + ".sdk",
			dir + "/Developer/Library/Frameworks",
			dir + "/Developer/Library/PrivateFrameworks",
			dir + "/Developer/usr/lib",
			dir + "/Developer/Library/Xcode/Agents",
			dir + "/Library/Application Support/MessagesApplicationStub",
			dir + "/Library/Application Support/MessagesApplicationExtensionStub",
		},
		exclude:   regexp.MustCompile(`^` + regexp.QuoteMeta(dir) + `/Developer/(usr/lib/swift/host|SDKs/[^/]+\.sdk/usr/share/man)(/|$)`),
		stampDirs: []string{dir + "/Developer/SDKs/*.sdk/usr/lib/swift", dir + "/Developer/SDKs/*.sdk/System/Library/Frameworks"},
		infoPlist: dir + "/Info.plist",
	}
}

var windowsReservedRe = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])(\.|$)`)

func validOnWindows(zp string) bool {
	for _, seg := range strings.Split(strings.TrimSuffix(zp, "/"), "/") {
		if seg == "" || strings.ContainsAny(seg, `<>:"|?*\`) || strings.HasSuffix(seg, " ") ||
			(strings.HasSuffix(seg, ".") && seg != "." && seg != "..") || windowsReservedRe.MatchString(seg) {
			return false
		}
		for _, r := range seg {
			if r < 0x20 {
				return false
			}
		}
	}
	return true
}

type packer struct {
	toolchain    *target
	platforms    []*target
	all          []*target
	xcodeVersion []byte
	xcodeMtime   time.Time
}

func newPacker(outDir string, platforms []string) *packer {
	p := &packer{toolchain: toolchainTarget(platforms)}
	p.toolchain.out = newZipOut(filepath.Join(outDir, "toolchain.zip"))
	p.all = append(p.all, p.toolchain)
	for _, name := range platforms {
		t := platformTarget(name)
		t.out = newZipOut(filepath.Join(outDir, name+".platform.zip"))
		p.platforms = append(p.platforms, t)
		p.all = append(p.all, t)
	}
	return p
}

func (p *packer) want(dir string) bool {
	if dir == "" || strings.HasPrefix(xcodeVersionPlist, dir+"/") {
		return true
	}
	for _, t := range p.all {
		if t.want(dir) {
			return true
		}
	}
	return false
}

func (p *packer) handle(e *entry) error {
	if e.path == xcodeVersionPlist && e.mode.IsRegular() {
		data, err := io.ReadAll(io.LimitReader(e.body, 1<<20))
		if err != nil {
			return err
		}
		p.xcodeVersion, p.xcodeMtime = data, e.mtime
		return nil
	}
	for _, t := range p.all {
		if !strings.HasPrefix(e.path, t.root+"/") {
			continue
		}
		zp, matched := t.match(e)
		stamp := t.wantsStamp(e)
		if !matched {
			if stamp {
				head := make([]byte, stampHeadSize)
				n, _ := io.ReadFull(e.body, head)
				t.takeStamp(head[:n])
			}
			if e.mode.IsDir() && t.want(e.path) {
				t.out.rememberDir(strings.TrimPrefix(e.path, t.root+"/"), e)
			}
			continue
		}
		if !validOnWindows(zp) {
			t.badNames = append(t.badNames, zp)
			return nil
		}
		switch {
		case e.mode.IsDir():
			return t.out.dir(zp, e.mode, e.mtime)
		case e.mode&fs.ModeSymlink != 0:
			return t.out.link(zp, e.link, e.mtime)
		case e.mode.IsRegular():
			var capture *headCapture
			if stamp {
				capture = &headCapture{limit: stampHeadSize}
			} else if zp == t.infoPlist {
				capture = &headCapture{limit: 1 << 20}
			}
			body := e.body
			if capture != nil {
				body = io.TeeReader(body, capture)
			}
			if err := t.out.file(zp, e.mode, e.mtime, body); err != nil {
				return err
			}
			if stamp {
				t.takeStamp(capture.buf.Bytes())
			} else if capture != nil {
				t.info = capture.buf.Bytes()
			}
		}
		return nil
	}
	return nil
}

func (p *packer) finish() ([]Product, error) {
	tc := p.toolchain
	if tc.swift == "" {
		return nil, fmt.Errorf("cannot tell the swift version of the toolchain: no .swiftinterface with a compiler-version stamp under %s/usr/lib/swift", toolchainRoot)
	}
	log.Info("toolchain swift version (from its own .swiftinterface stamps)", "swift", tc.swift)
	if err := tc.out.finish(); err != nil {
		return nil, fmt.Errorf("toolchain pack failed: %w", err)
	}
	log.Info("written", "zip", tc.out.path)
	for _, t := range p.all {
		if len(t.badNames) > 0 {
			log.Warn("skipped entries with non-portable file names",
				"zip", t.name, "count", len(t.badNames), "first", t.badNames[0])
		}
	}
	products := []Product{{Zip: tc.out.path, Name: "toolchain", Version: tc.swift, Swift: tc.swift}}

	if p.xcodeVersion == nil {
		log.Warn("xcode version file not found; skipped", "file", xcodeVersionPlist)
	}
	hostSystemVersion, hostErr := os.ReadFile(hostSystemVersionPlist)
	if hostErr != nil {
		hostSystemVersion = defaultSystemVersion
		log.Info("no local SystemVersion.plist; using the built-in one")
	}
	var failed []string
	for _, t := range p.platforms {
		prod, err := p.finishPlatform(t, tc.swift, hostSystemVersion)
		if err != nil {
			log.Error("platform pack failed", "platform", t.name, "error", err)
			t.out.abort()
			failed = append(failed, t.name)
			continue
		}
		log.Info("written", "zip", t.out.path)
		products = append(products, prod)
	}
	if len(failed) > 0 {
		return products, fmt.Errorf("sdk pack failed for %s", strings.Join(failed, ", "))
	}
	return products, nil
}

func (p *packer) finishPlatform(t *target, toolchainSwift string, hostSystemVersion []byte) (Product, error) {
	if t.info == nil {
		return Product{}, fmt.Errorf("%s/%s not found in this Xcode", t.root, t.infoPlist)
	}
	dir := t.name + ".platform"
	prod := Product{Zip: t.out.path, Name: dir, Version: "1.0.0"}
	var info map[string]any
	if _, err := plist.Unmarshal(t.info, &info); err == nil {
		if v, ok := info["Version"].(string); ok {
			prod.Version = v
		}
	}
	if t.swift != "" {
		prod.Swift = t.swift
		log.Info("detected SDK swift overlay version", "sdk", dir, "swift", t.swift)

		if t.swift != toolchainSwift {
			log.Warn("SDK overlay swift version != packaged toolchain swift version (inconsistent Xcode?)",
				"sdk", dir, "sdkSwift", t.swift, "toolchainSwift", toolchainSwift)
		}
	} else {
		log.Warn("no swift overlay found in SDK; using the toolchain's swift version", "sdk", dir)
		prod.Swift = toolchainSwift
	}

	if p.xcodeVersion != nil {
		if err := t.out.file(dir+"/"+XcodeInfoDirName+"/version.plist", 0o644, p.xcodeMtime, bytes.NewReader(p.xcodeVersion)); err != nil {
			return Product{}, err
		}
	}
	if err := t.out.file(dir+"/"+XcodeInfoDirName+"/SystemVersion.plist", 0o644, time.Now(), bytes.NewReader(hostSystemVersion)); err != nil {
		return Product{}, err
	}
	return prod, t.out.finish()
}

func (p *packer) abortAll() {
	for _, t := range p.all {
		t.out.abort()
	}
}

type headCapture struct {
	buf   bytes.Buffer
	limit int
}

func (c *headCapture) Write(b []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		c.buf.Write(b[:min(room, len(b))])
	}
	return len(b), nil
}
