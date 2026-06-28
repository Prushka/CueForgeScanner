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
| `CUEFORGE_OUTPUT_FORMATS`   | No       | `ass,vtt,srt`                     | Comma separated CueForge output formats to request and save.                                                                     |
| `CUEFORGE_REQUEST_TIMEOUT`  | No       | no timeout                        | Request timeout as a Go duration such as `30m` or `1800s`.                                                                      |
| `CUEFORGE_CONCURRENCY`      | No       | `1`                               | Maximum number of concurrent CueForge translation requests across all folders and target languages.                              |
| `CUEFORGE_SKIP_EXISTING_TARGET_FILES` | No | `true`                            | When `true`, existing target-language files can skip a target. Set to `false` to translate and overwrite expected outputs.       |
| `CUEFORGE_SKIP_GENERATED_AFTER_UNIX` | No | `0`                               | Unix timestamp in seconds. Existing outputs only skip a target when every expected file exists and was modified after this time. |
| `SAVE_ON_ERROR`             | No       | `false`                           | When `true`, failed CueForge requests save the uploaded subtitle and error metadata for debugging.                                |
| `ERROR_DIR`                 | No       | `./errors`                        | Directory where `SAVE_ON_ERROR` writes failed subtitle snapshots.                                                                |

## Usage

```sh
CUEFORGE_INPUT_LANGUAGES='eng,ger,chi' \
CUEFORGE_TARGET_LANGUAGES='chi,$jpn' \
CUEFORGE_REASONING_EFFORT=medium \
CUEFORGE_OUTPUT_FORMATS='ass,vtt,srt' \
CUEFORGE_CONCURRENCY=2 \
go run ./cmd/scanner
```

The scanner processes child folders from newest to oldest by folder modification time, with up to `CUEFORGE_CONCURRENCY` CueForge translation requests active globally across folders and target languages. Target languages inside each active folder are scheduled concurrently; targets resolving to the same output language are serialized so their output files do not race. It uploads one selected subtitle using format order `ass`, `vtt`, `sup`, then `sub`; when multiple candidates have the same language and format, the largest file is preferred. It requests the `CUEFORGE_OUTPUT_FORMATS` values from CueForge and writes those formats. When `CUEFORGE_SKIP_EXISTING_TARGET_FILES` is true, a target is skipped when every file it would generate already exists and every file's modification time is after `CUEFORGE_SKIP_GENERATED_AFTER_UNIX`; when the timestamp env var is empty or unset, the cutoff defaults to Unix `0`. For text inputs with skip enabled, if a plain target-language subtitle already exists in the folder, such as `cueforge_chi.ass` or a source-style `2-chi.ass`, unannotated targets are skipped; annotated targets upload that existing target-language subtitle and write only the annotated configured output formats. Set `CUEFORGE_SKIP_EXISTING_TARGET_FILES=false` to bypass those existing-file skips and overwrite expected outputs. OCR/image subtitle inputs (`.sup`, or `.sub` with `.idx`) keep the normal translation and OCR-original behavior. If a folder has a readable `job.json`, `media.title` is sent as CueForge's `media` form field for translations from that folder; when `media.title` is missing or blank, the extensionless `input` filename is used instead. When `SAVE_ON_ERROR` is enabled and a CueForge request fails, the uploaded subtitle is copied under `ERROR_DIR/<job title>/` when `job.json` provides a title, otherwise under `ERROR_DIR/<original folder>/`; a target-specific sidecar such as `2-eng.ass.jpn.json` or `2-eng.ass.jpn.annotated.json` records the target language, annotation flag, request parameters, and error response.

For a target such as `jpn`, files are written as:

```text
cueforge_jpn.ass
cueforge_jpn.vtt
cueforge_jpn.srt
cueforge_jpn_annotated.ass
cueforge_jpn_annotated.vtt
cueforge_jpn_annotated.srt
```

Annotated files are only written for targets prefixed with `$`.

For image subtitle inputs such as `4-eng.sup`, or VobSub `.sub` inputs with a matching `.idx`, CueForge returns OCR'd original-language subtitles too. Those are written as:

```text
cueforge_eng.ass
cueforge_eng.vtt
cueforge_eng.srt
```
