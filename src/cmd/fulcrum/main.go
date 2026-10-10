// Command fulcrum downloads the GeoJSON of a Fulcrum data share and converts
// it to an Esri shapefile.
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
// Progress is reported on standard error and a summary on standard output.
// The exit status is 0 on success and 1 on any error.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/mikey1964x/fulcrum/pkg/config"
	"github.com/mikey1964x/fulcrum/pkg/datashare"
	"github.com/mikey1964x/fulcrum/pkg/shapefile"
)

// main runs the command and exits with status 1 if it fails.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run parses the flags, loads the configuration, downloads the share,
// converts it and prints the summary. Interrupting the process (Ctrl+C)
// cancels a download in progress.
func run() error {
	envPath := flag.String("env", ".env", "path of the .env file")
	outDir := flag.String("out", "", "output directory (default: FULCRUM_OUTPUT_DIR or \"data\")")
	name := flag.String("name", "", "base name of the output files (default: the share token)")
	keepGeoJSON := flag.Bool("keep-geojson", true, "also save the downloaded GeoJSON")
	flag.Parse()

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
