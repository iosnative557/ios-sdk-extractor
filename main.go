package main

import (
	"fmt"
	log "log/slog"
	"os"
	"runtime/debug"

	"github.com/iosnative557/ios-sdk-extractor/extract"
	"github.com/spf13/cobra"
)

var Version = ""

func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	var (
		platform    string
		output      string
		xcode       string
		showVersion bool
	)
	root := &cobra.Command{
		Use:   "ios-sdk-extractor",
		Short: "Extract the iOS SDK and toolchain libraries from Xcode",
		Long: `Extract the iOS SDK and toolchain libraries from Xcode.app or an Xcode .xip.

Examples:
  ios-sdk-extractor
  ios-sdk-extractor -x Xcode_27.xip
  ios-sdk-extractor -x Xcode_27.xip -c "iphone watch" -o sdkextracted`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		Args:              cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVersion {
				fmt.Println("ios-sdk-extractor " + version())
				return nil
			}
			products, err := extract.Run(extract.Options{Platform: platform, Output: output, Xcode: xcode})
			if err != nil {
				return err
			}
			swift := ""
			for _, p := range products {
				log.Info("packed", "zip", p.Zip, "component", p.Name, "version", p.Version)
				if p.Name == "iPhoneOS.platform" || swift == "" {
					swift = p.Swift
				}
			}
			log.Info("done; build with Swift "+swift+" to use this SDK", "output", output)
			return nil
		},
	}
	f := root.Flags()
	f.StringVarP(&platform, "platform", "c", "iphone", "Platforms, space separated: iphone watch appletv xros mac")
	f.StringVarP(&output, "output", "o", "./sdkextracted", "Output directory")
	f.StringVarP(&xcode, "xcode", "x", "/Applications/Xcode.app", "Xcode.app or Xcode .xip file")
	f.BoolVar(&showVersion, "version", false, "Print the version")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR", err)
		os.Exit(1)
	}
}
