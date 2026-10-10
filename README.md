# fulcrum

Console command that downloads the GeoJSON of a [Fulcrum](https://www.fulcrumapp.com/) data share and converts it to an Esri shapefile.

## Setup

1. Copy `.env.example` to `.env` and set `FULCRUM_SHARE_URL` to the `.geojson` link of the data share.
2. Build the command:

   ``` bash
   go build -o bin/fulcrum.exe ./src/cmd/fulcrum
   ```

`.env` settings:

| Variable | Required | Meaning |
| --- | --- | --- |
| `FULCRUM_SHARE_URL` | yes | `https://web.fulcrumapp.com/shares/<token>.geojson` |
| `FULCRUM_USERNAME`, `FULCRUM_PASSWORD` | no | Fulcrum login. Only sent, as HTTP Basic auth, if the server refuses an anonymous request. |
| `FULCRUM_OUTPUT_DIR` | no | Output directory, default `data`. |

Variables already set in the environment take precedence over `.env`.

## Usage

``` bash
bin\fulcrum.exe [-env .env] [-out <dir>] [-name <basename>] [-keep-geojson=false]
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-env` | `.env` | Path of the settings file. |
| `-out` | `FULCRUM_OUTPUT_DIR` | Output directory. |
| `-name` | share token | Base name of the output files. |
| `-keep-geojson` | `true` | Also save the downloaded GeoJSON. |

Fulcrum serves a share in pages of 20,000 records; the command requests every page and merges them.

## Output

For a share named `<name>`:

| File | Content |
| --- | --- |
| `<name>.shp`, `.shx`, `.dbf` | The shapefile. |
| `<name>.prj` | Coordinate system: WGS 84 (EPSG:4326). |
| `<name>.cpg` | Text encoding of the table: UTF-8. |
| `<name>_fields.csv` | Shapefile column name for every Fulcrum field. |
| `<name>.geojson` | All pages of the download merged into one file. |

A shapefile holds one geometry type. If a share mixes types, one shapefile per type is written: `<name>_point`, `<name>_multipoint`, `<name>_line`, `<name>_polygon`. Records without a location are skipped and counted in the summary.

## Shapefile limits

- **Column names are at most 10 characters.** Longer Fulcrum field names are truncated, and names that would collide get a numeric suffix (`xfmr_photo`, `xfmr_pho_1`, `xfmr_pho_2`). `<name>_fields.csv` lists the mapping.
- **Text is at most 254 bytes.** Longer values are truncated; the summary reports how many.
- **Dates and times are stored as text**, exactly as Fulcrum sends them.
- A column is numeric only if every value in the share is a number; otherwise it is text.

## Development

``` bash
go vet ./...
go test ./...
```

### Continuous integration

`.github/workflows/ci.yml` runs on every push to `main` and on every pull request:

| Job | What it does |
| --- | --- |
| Format, tidy and vet | `gofmt`, `go mod tidy -diff`, `go mod verify`, `go vet` |
| Test | `go test` on Linux (with the race detector and coverage) and Windows |
| Vulnerability scan | `govulncheck` |
| Build | Cross-compiles for Windows, Linux and macOS and uploads the binaries as artifacts |

Pushing a tag such as `v1.0.0` also publishes those binaries, with checksums, as a GitHub release once every job has passed.
