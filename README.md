# ios-sdk-extractor

A small command-line tool that extracts the **iOS SDK** (and the watchOS, tvOS, visionOS and
macOS SDKs) together with the **Swift / clang toolchain libraries** from Xcode, and packs them
into portable zip files.

## What need


Two kinds of input are supported:

- **Xcode.app**: an installed Xcode.
- **Xcode_\*.xip**: the Xcode archive downloaded from
  [developer.apple.com/download](https://developer.apple.com/download/all/). The xip is
  read in a single streaming pass, without unpacking the 10+ GB Xcode to disk; decompression uses all CPU cores (Xcode 27: about 50 seconds on a 6-core PC).

## Prebuilt SDK

Don't want to download Xcode? [ios-sdk](https://github.com/iosnative557/ios-sdk) has
[iPhoneOS.platform.zip](https://github.com/iosnative557/ios-sdk) and [toolchain.zip](https://github.com/iosnative557/ios-sdk) extracted with this tool (Xcode 27.0, iOS SDK
27.0, Swift 6.4.0), with instructions for using them.

## Build

Requires [Go](https://go.dev/dl/) 1.22 or newer. Pure Go, no cgo.

Install the latest release into `$(go env GOPATH)/bin`:

```sh
go install github.com/iosnative557/ios-sdk-extractor@latest
```

Or build from source:

```sh
git clone https://github.com/iosnative557/ios-sdk-extractor.git
cd ios-sdk-extractor
go build -trimpath -o ios-sdk-extractor .
```

Prebuilt binaries are attached to each
[release](https://github.com/iosnative557/ios-sdk-extractor/releases).

## Usage

```
ios-sdk-extractor [-x <Xcode.app | Xcode.xip>] [-c <platforms>] [-o <output dir>]
```

| Flag | Default | Description |
|---|---|---|
| `-x, --xcode` | `/Applications/Xcode.app` | Xcode.app directory or Xcode `.xip` file |
| `-c, --platform` | `iphone` | Platforms, space separated: `iphone` `watch` `appletv` `xros` `mac` |
| `-o, --output` | `./sdkextracted` | Output directory (created if missing) |
| `--version` | | Print the version |

Examples:

```sh
# from the installed Xcode
ios-sdk-extractor

# from a downloaded xip
ios-sdk-extractor -x Xcode_27.xip -o sdkextracted

# iOS + watchOS
ios-sdk-extractor -x Xcode_27.xip -c "iphone watch"
```

When it finishes, the tool prints the Swift version the SDK was built with (e.g. `swift=6.4.0`).
The Swift toolchain that compiles against this SDK must be the same version.

## Output

```
sdkextracted/
├── toolchain.zip
├── iPhoneOS.platform.zip
└── ...                      one <Platform>.platform.zip per selected platform
```

Typical sizes for Xcode 27: toolchain 25 MB, iPhoneOS 46 MB, WatchOS 57 MB, AppleTVOS 47 MB,
XROS 43 MB, MacOSX 139 MB.

## Use as a Go library

The extractor is also importable: package `extract` does everything the command does.

```go
import "github.com/iosnative557/ios-sdk-extractor/extract"

products, err := extract.Run(extract.Options{
    Xcode:    "Xcode_27.xip",   // Xcode.app directory or .xip file
    Platform: "iphone",         // same syntax as -c
    Output:   "sdkextracted",   // created if missing
})
for _, p := range products {
    fmt.Println(p.Zip, p.Name, p.Version, p.Swift)
}
```

Progress is reported through `log/slog`.

## License

Apache License 2.0, see [LICENSE](LICENSE).

This tool contains no Apple software. Xcode and the SDKs are provided by Apple under the
Xcode and Apple SDKs Agreement; use the extracted files in accordance with it.
