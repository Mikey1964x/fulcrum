# fulcrum

Console command that downloads the GeoJSON of a [Fulcrum](https://www.fulcrumapp.com/) data share, converts it to an Esri shapefile and downloads the photos attached to its records. It can also serve a dashboard website with the inspection statistics of the downloaded share.

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
| `FULCRUM_USERNAME`, `FULCRUM_PASSWORD` | no | Fulcrum login, for the share and its photos. Only sent, as HTTP Basic auth, if the server refuses an anonymous request. |
| `FULCRUM_OUTPUT_DIR` | no | Output directory, default `data`. |

Variables already set in the environment take precedence over `.env`.

## Usage

``` bash
bin\fulcrum.exe [-env .env] [-out <dir>] [-name <basename>] [-keep-geojson=false] [-photos] [-photo-workers <n>]
bin\fulcrum.exe -serve [-addr localhost:8080]
bin\fulcrum.exe -help
bin\fulcrum.exe -version
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-env` | `.env` | Path of the settings file. |
| `-out` | `FULCRUM_OUTPUT_DIR` | Output directory. |
| `-name` | share token | Base name of the output files. |
| `-keep-geojson` | `true` | Also save the downloaded GeoJSON. |
| `-photos` | off | Also download the photos the records link to. |
| `-photo-workers` | `8` | Number of photos downloaded in parallel. |
| `-serve` | | Serve the dashboard of the share already downloaded, instead of downloading it. |
| `-addr` | `localhost:8080` | Address the dashboard listens on. |
| `-h`, `-help` | | Print the usage and exit. |
| `-v`, `-version` | | Print the version and exit. |

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
| `photos/<guid>.jpg` | One file per photo, named after the photo's GUID. Only with `-photos`. |

A shapefile holds one geometry type. If a share mixes types, one shapefile per type is written: `<name>_point`, `<name>_multipoint`, `<name>_line`, `<name>_polygon`. Records without a location are skipped and counted in the summary.

## Photos

A photo field comes with an attribute whose name ends in `_url`, such as `xfmr_photo_url`. Its value links to a web page showing the record's photos:

```
https://web.fulcrumapp.com/shares/<token>/photos/view?photos=<guid>,<guid>
```

Photos are only downloaded when the command is run with `-photos`. It then reads the GUIDs from every such attribute and downloads each picture once from `https://web.fulcrumapp.com/shares/<token>/photos/<guid>` into the `photos` subdirectory of the output directory. No API token is needed: the share token in the link is the access control, as it is for the data.

- **Reruns only fetch what is new.** A photo whose file is already in `photos` is skipped, so an interrupted run (Ctrl+C) can simply be started again.
- **A large share takes a while and a lot of disk space.** Photos are roughly 350 KB each.
- A photo that has been deleted in Fulcrum is counted as missing. A photo that fails to download is retried once, then counted as failed; the command finishes the others and exits with status 1.
- `_url` values that are not photo links of the share's own site are not requested, and are counted in the summary.

## Dashboard

``` bash
bin\fulcrum.exe -serve
```

starts a web server and prints its address, `http://localhost:8080`. Open that in a browser; stop the server with Ctrl+C. Nothing is downloaded from Fulcrum: the dashboard reads the `<name>.geojson` in the output directory, so run the command without `-serve` first. A later download is picked up without restarting the server.

The page shows:

- the share of units inspected, and the number inspected, not inspected and in any other status;
- the units per status, and the progress per inspection type and per county;
- what the inspections found: exterior, clearance, penta bolt, lock, label and oil leak;
- the units per month of their inspection date.

Every figure can be narrowed to one year, inspection type, county or status with the filters at the top. The **Year** list offers the current year and the four before it, plus "All years (total)" for the overall view. A unit belongs to the year of its `updated_at` attribute, the last time its record was changed in Fulcrum, so each unit is counted in one year only. The page is built into the command and loads nothing from the internet.

The **View map** button opens `/map`, which draws every unit that has a location, coloured by its `marker-color` attribute, with a legend of the statuses. Click a unit to see all its attributes. Unlike the dashboard page, the map needs internet access in the browser: it loads [Leaflet](https://leafletjs.com/) from unpkg and the base map tiles from OpenStreetMap.

The **Download GeoJSON** button saves `<name>.geojson` to the computer the browser runs on.

The server only accepts connections from the same computer. To let others on the network open the dashboard, start it with `-addr :8080`. It has no login, so anyone who can reach that port can see the statistics, the map and every unit's attributes, and download the data.

### Running the dashboard as a Windows service

`scripts\install-service.ps1` installs the dashboard as a service named `FulcrumDashboard`, using [NSSM](https://nssm.cc). Run it from an elevated PowerShell, after building the command and downloading the share once:

``` powershell
scripts\install-service.ps1                # install and start, on localhost:8080
scripts\install-service.ps1 -Addr :8080    # serve the network instead
scripts\install-service.ps1 -Remove        # stop and remove the service
```

The service runs as `LOCAL SERVICE`, starts with Windows, and uses the executable's directory as its working directory. Output is logged there in `FulcrumDashboard.out.log` and `FulcrumDashboard.err.log`. The script grants `LOCAL SERVICE` read access to the executable directory (with write, for the logs), the `.env` file and the data directory, since that account cannot otherwise read a user's files.

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

### Release builds

`scripts/build.sh` (bash) and `scripts\build.ps1` (PowerShell) build the command the way the release job does: a static binary without debug information, stamped with its version.

``` bash
scripts/build.sh                      # this machine's binary, into bin/
scripts/build.sh --all                # windows, linux and macOS binaries plus SHA256SUMS, into dist/
scripts/build.sh --version v1.2.3     # set the version reported by -version
```

``` powershell
scripts\build.ps1
scripts\build.ps1 -All
scripts\build.ps1 -Version v1.2.3
```

Without a version, `git describe` supplies one, such as `v0.3.2` on a tagged commit or `v0.3.2-3-g1a2b3c4-dirty` after it.

### Continuous integration

`.github/workflows/ci.yml` runs on every push to `main` and on every pull request:

| Job | What it does |
| --- | --- |
| Format, tidy and vet | `gofmt`, `go mod tidy -diff`, `go mod verify`, `go vet` |
| Test | `go test` on Linux (with the race detector and coverage) and Windows |
| Vulnerability scan | `govulncheck` |
| Build | Cross-compiles for Windows, Linux and macOS and uploads the binaries as artifacts |

Pushing a tag such as `v1.0.0` also publishes those binaries, with checksums, as a GitHub release once every job has passed.
