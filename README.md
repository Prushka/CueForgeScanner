# CueForgeScanner

Scans folders containing subtitle exports, picks one source subtitle file per folder, sends it to CueForge, and writes translated outputs back into the same folder.

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
| `CUEFORGE_CONCURRENCY`      | No       | `1`                               | Maximum number of folders processed concurrently.                                                                                |

## Usage

```sh
CUEFORGE_INPUT_LANGUAGES='eng,ger,chi' \
CUEFORGE_TARGET_LANGUAGES='chi,$jpn' \
CUEFORGE_REASONING_EFFORT=medium \
CUEFORGE_CONCURRENCY=2 \
go run ./cmd/scanner
```

The scanner processes child folders from newest to oldest by folder modification time, with up to `CUEFORGE_CONCURRENCY` folders active at once. It uploads one selected subtitle using format order `ass`, `vtt`, `sup`, then `sub`, and always requests `ass` and `vtt` output formats from CueForge. If every file a target would generate already exists, that target is skipped. If a folder has a readable `job.json` with `media.title`, that title is sent as CueForge's `media` form field for translations from that folder.

For a target such as `jpn`, files are written as:

```text
cueforge_jpn.ass
cueforge_jpn.vtt
cueforge_jpn_annotated.ass
cueforge_jpn_annotated.vtt
```

Annotated files are only written for targets prefixed with `$`.

For image subtitle inputs such as `4-eng.sup`, or VobSub `.sub` inputs with a matching `.idx`, CueForge returns OCR'd original-language subtitles too. Those are written as:

```text
cueforge_eng.ass
cueforge_eng.vtt
```
