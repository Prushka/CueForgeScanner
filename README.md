# CueForgeScanner

Scans folders containing subtitle exports, picks one source `.ass` file per folder, sends it to CueForge, and writes translated outputs back into the same folder.

The scanner fetches CueForge's `GET /languages` registry at startup and uses that response for language ID/name validation and alias matching.

## Configuration

The scanner is configured with environment variables:

| Variable                    | Required | Default                           | Description                                                                                                                     |
|-----------------------------|----------|-----------------------------------|---------------------------------------------------------------------------------------------------------------------------------|
| `CUEFORGE_SCAN_DIR`         | No       | `~/GolandProjects/Sparkle/output` | Directory whose direct child folders should be scanned.                                                                         |
| `CUEFORGE_BASE_URL`         | No       | `http://localhost:8080`           | CueForge server base URL.                                                                                                       |
| `CUEFORGE_INPUT_LANGUAGES`  | Yes      |                                   | Comma separated source language priorities. CueForge language IDs and names are accepted. Earlier entries have higher priority. |
| `CUEFORGE_TARGET_LANGUAGES` | Yes      |                                   | Comma separated target languages. Prefix a target with `$` to request annotated subtitles too.                                  |
| `CUEFORGE_MODEL`            | No       |                                   | Optional CueForge `model` form field.                                                                                           |
| `CUEFORGE_VMODEL`           | No       |                                   | Optional CueForge `vmodel` form field.                                                                                          |
| `CUEFORGE_REASONING_EFFORT` | No       |                                   | Optional CueForge `reasoning_effort` form field.                                                                                |
| `CUEFORGE_REQUEST_TIMEOUT`  | No       | no timeout                        | Request timeout as a Go duration such as `30m` or `1800s`.                                                                      |

## Usage

```sh
CUEFORGE_INPUT_LANGUAGES='eng,ger,chi' \
CUEFORGE_TARGET_LANGUAGES='chi,$jpn' \
CUEFORGE_REASONING_EFFORT=medium \
go run ./cmd/scanner
```

The scanner processes child folders from newest to oldest by folder modification time. It uploads only the selected `.ass` subtitle and always requests `ass` and `vtt` output formats from CueForge. If a folder has a readable `job.json` with `media.title`, that title is sent as CueForge's `media` form field for translations from that folder.

For a target such as `jpn`, files are written as:

```text
cueforge_jpn.ass
cueforge_jpn.vtt
cueforge_jpn_annotated.ass
cueforge_jpn_annotated.vtt
```

Annotated files are only written for targets prefixed with `$`.
