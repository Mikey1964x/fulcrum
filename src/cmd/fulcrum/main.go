// Command fulcrum downloads the GeoJSON of a Fulcrum data share, converts it
// to an Esri shapefile and downloads the photos attached to its records.
//
// Usage:
//
//	fulcrum [flags]
//
// The flags are:
//
//	-env file
//		Path of the .env file with the settings (default ".env").
//	-out dir
//		Output directory (default: FULCRUM_OUTPUT_DIR, or "data").
//	-name basename
//		Base name of the output files (default: the share token).
//	-keep-geojson
//		Also save the downloaded GeoJSON (default true).
//	-photos
//		Download the photos the records link to (default true).
//	-photo-workers n
//		Number of photos downloaded in parallel (default 8).
//	-serve
//		Serve the dashboard of the share already downloaded, instead of
//		downloading it.
//	-addr host:port
//		Address the dashboard listens on (default "localhost:8080").
//	-h, -help
//		Print the usage and exit.
//	-v, -version
//		Print the version and exit.
//
// The share to download is configured through environment variables, which
// are usually set in the .env file:
//
//	FULCRUM_SHARE_URL    the .geojson link of the data share (required)
//	FULCRUM_USERNAME     Fulcrum login, sent only if the server asks for it
//	FULCRUM_PASSWORD     password for that login
//	FULCRUM_OUTPUT_DIR   output directory
//
// Every page of the share is downloaded and written to the output directory
// as <name>.shp with its .shx, .dbf, .prj and .cpg companions, plus
// <name>_fields.csv, which maps the shapefile's shortened column names back
// to the Fulcrum field names. If the share mixes geometry types, one
// shapefile per type is written.
//
// The photos linked from attributes whose name ends in "_url" are saved in
// the "photos" subdirectory of the output directory, each named after its
// GUID. Photos already there are not downloaded again, so an interrupted run
// can be repeated to fetch the rest.
//
// With -serve nothing is downloaded. The command instead serves a web page
// with the statistics of the GeoJSON in the output directory, and links to
// download that GeoJSON alone or in a zip archive with the photos. It runs
// until interrupted.
//
// Progress is reported on standard error and a summary on standard output.
// The exit status is 0 on success and 1 on any error, including a photo that
// could not be downloaded.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/config"
	"github.com/mikey1964x/fulcrum/pkg/dashboard"
	"github.com/mikey1964x/fulcrum/pkg/datashare"
	"github.com/mikey1964x/fulcrum/pkg/photos"
	"github.com/mikey1964x/fulcrum/pkg/shapefile"
)

// main runs the command and exits with status 1 if it fails.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// photosDir is the subdirectory of the output directory the photos are
// saved in.
const photosDir = "photos"

// version is the release the command was built from, such as "v0.1.0". It
// is set at build time with -ldflags "-X main.version=...". If it is empty,
// the version recorded by the Go toolchain is reported instead.
var version = ""

// usage is the text printed for -h and -help, and after an invalid flag.
const usage = `fulcrum downloads the GeoJSON of a Fulcrum data share, converts it to an
Esri shapefile and downloads the photos attached to its records.

Usage:

  fulcrum [flags]

Flags:

  -env file          path of the .env file with the settings (default ".env")
  -out dir           output directory (default: FULCRUM_OUTPUT_DIR, or "data")
  -name basename     base name of the output files (default: the share token)
  -keep-geojson      also save the downloaded GeoJSON (default true)
  -photos            download the photos the records link to (default true)
  -photo-workers n   number of photos downloaded in parallel (default 8)
  -serve             serve the dashboard of the share already downloaded,
                     instead of downloading it
  -addr host:port    address the dashboard listens on (default "localhost:8080")
  -h, -help          print this usage and exit
  -v, -version       print the version and exit

  Turn an on/off flag off with =false, for example -photos=false.

Settings, read from the environment and the .env file:

  FULCRUM_SHARE_URL    the .geojson link of the data share (required)
  FULCRUM_USERNAME     Fulcrum login, sent only if the server asks for it
  FULCRUM_PASSWORD     password for that login
  FULCRUM_OUTPUT_DIR   output directory (default "data")
`

// versionString returns the line printed for -v and -version, for example
// "fulcrum v0.1.0 (go1.26.4, windows/amd64)".
func versionString() string {
	v := version
	if v == "" {
		v = "(unknown version)"
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
			v = info.Main.Version
		}
	}
	return fmt.Sprintf("fulcrum %s (%s, %s/%s)", v, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// run parses the flags, loads the configuration, downloads the share,
// converts it, downloads the photos and prints the summary. Interrupting the
// process (Ctrl+C) cancels a download in progress.
func run() error {
	envPath := flag.String("env", ".env", "path of the .env file")
	outDir := flag.String("out", "", "output directory (default: FULCRUM_OUTPUT_DIR or \"data\")")
	name := flag.String("name", "", "base name of the output files (default: the share token)")
	keepGeoJSON := flag.Bool("keep-geojson", true, "also save the downloaded GeoJSON")
	getPhotos := flag.Bool("photos", true, "download the photos the records link to")
	photoWorkers := flag.Int("photo-workers", 8, "number of photos downloaded in parallel")
	serve := flag.Bool("serve", false, "serve the dashboard of the share already downloaded")
	addr := flag.String("addr", "localhost:8080", "address the dashboard listens on")
	var showHelp, showVersion bool
	flag.BoolVar(&showHelp, "h", false, "print the usage and exit")
	flag.BoolVar(&showHelp, "help", false, "print the usage and exit")
	flag.BoolVar(&showVersion, "v", false, "print the version and exit")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Usage = func() { fmt.Fprint(flag.CommandLine.Output(), usage) }
	flag.Parse()

	if showHelp {
		fmt.Print(usage)
		return nil
	}
	if showVersion {
		fmt.Println(versionString())
		return nil
	}

	cfg, err := config.Load(*envPath)
	if err != nil {
		return err
	}
	if *outDir != "" {
		cfg.OutputDir = *outDir
	}
	if *name == "" {
		*name = cfg.ShareName()
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *serve {
		return serveDashboard(ctx, cfg, *name, *addr)
	}

	client := &datashare.Client{
		URL:      cfg.ShareURL,
		Username: cfg.Username,
		Password: cfg.Password,
		HTTP:     &http.Client{Timeout: 15 * time.Minute},
		Progress: os.Stderr,
	}

	fmt.Fprintln(os.Stderr, "downloading data share...")
	geojsonPath := filepath.Join(cfg.OutputDir, *name+".geojson")
	result, err := download(ctx, client, geojsonPath, *keepGeoJSON)
	if err != nil {
		return err
	}
	if len(result.Features) == 0 {
		return fmt.Errorf("the share returned no features")
	}

	fmt.Fprintln(os.Stderr, "writing shapefile...")
	sum, err := shapefile.Write(result.Features, cfg.OutputDir, *name)
	if err != nil {
		return err
	}

	// The photos come last, so the data is already safe on disk if the long
	// photo download is interrupted.
	var (
		photoResult  *photos.Result
		photoSkipped int
		photoErr     error
	)
	photoPath := filepath.Join(cfg.OutputDir, photosDir)
	if *getPhotos {
		var refs []photos.Ref
		refs, photoSkipped = photos.Collect(result.Features, cfg.ShareURL)
		fmt.Fprintln(os.Stderr, "downloading photos...")
		downloader := &photos.Downloader{
			Dir:      photoPath,
			Username: cfg.Username,
			Password: cfg.Password,
			HTTP:     &http.Client{Timeout: 2 * time.Minute},
			Workers:  *photoWorkers,
			Progress: os.Stderr,
		}
		photoResult, photoErr = downloader.Download(ctx, refs)
	}

	fmt.Printf("Downloaded %d features in %d page(s)\n", len(result.Features), result.Pages)
	if result.Duplicates > 0 {
		fmt.Printf("  %d duplicate features ignored\n", result.Duplicates)
	}
	if *keepGeoJSON {
		fmt.Printf("GeoJSON:   %s\n", geojsonPath)
	}
	for _, out := range sum.Outputs {
		fmt.Printf("Shapefile: %s (%s, %d features, %d fields)\n", out.Path, out.Geometry, out.Features, len(sum.Fields))
	}
	fmt.Printf("Field map: %s\n", sum.FieldMap)
	if sum.Skipped > 0 {
		fmt.Printf("Skipped %d features without a usable geometry\n", sum.Skipped)
	}
	if sum.Truncated > 0 {
		fmt.Printf("Truncated %d attribute values longer than the 254-byte shapefile limit\n", sum.Truncated)
	}
	if photoResult != nil {
		fmt.Printf("Photos:    %s (%d downloaded, %d already present, %d missing, %d failed)\n",
			photoPath, photoResult.Downloaded, photoResult.Existing, photoResult.Missing, photoResult.Failed)
		if photoSkipped > 0 {
			fmt.Printf("Skipped %d \"_url\" values that are not photo links of this share\n", photoSkipped)
		}
		for _, e := range photoResult.Errors {
			fmt.Fprintln(os.Stderr, "error:", e)
		}
		if photoErr != nil {
			return fmt.Errorf("downloading photos: %w", photoErr)
		}
		if photoResult.Failed > 0 {
			return fmt.Errorf("%d photos could not be downloaded; run again to retry them", photoResult.Failed)
		}
	}
	return nil
}

// download fetches the share, saving the merged GeoJSON to path when keep is
// set. A partial file is removed if the download fails.
func download(ctx context.Context, client *datashare.Client, path string, keep bool) (result *datashare.Result, err error) {
	if !keep {
		return client.Download(ctx, nil)
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := file.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	w := bufio.NewWriterSize(file, 1<<20)
	result, err = client.Download(ctx, io.Writer(w))
	if err != nil {
		return nil, err
	}
	return result, w.Flush()
}

// serveDashboard serves the dashboard of the share saved under name in the
// output directory, on addr, until ctx is done. It fails straight away if
// the GeoJSON is missing or cannot be read.
func serveDashboard(ctx context.Context, cfg *config.Config, name, addr string) error {
	dash := &dashboard.Server{
		Name:        name,
		GeoJSONPath: filepath.Join(cfg.OutputDir, name+".geojson"),
		PhotosDir:   filepath.Join(cfg.OutputDir, photosDir),
		ShareURL:    cfg.ShareURL,
	}
	fmt.Fprintln(os.Stderr, "reading", dash.GeoJSONPath)
	features, err := dash.Load()
	if err != nil {
		return err
	}

	// No write timeout: the zip download of the photos can take very long.
	srv := &http.Server{Addr: addr, Handler: dash.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	shown := addr
	if strings.HasPrefix(shown, ":") {
		shown = "localhost" + shown
	}
	fmt.Printf("Dashboard for %d features: http://%s (Ctrl+C to stop)\n", len(features), shown)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
