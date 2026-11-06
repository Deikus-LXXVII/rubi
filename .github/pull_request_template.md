## What and why

<!-- One or two sentences. -->

## Project rules

See [docs/project-rules.md](../docs/project-rules.md). Tick each line, or say why it does not apply.

- [ ] **1. Phone first.** Anything that needs the user (an approval, a sign-in, a setup step) can be finished from a phone in the panel; the layout also works at desktop width (ran `scripts/panel-viewport-check.md` if `panel/` changed).
- [ ] **2. No terminal.** No new text, doc or flow tells the user to run a command or open a terminal, on any of their devices (`TestNoTextTellsTheUserToUseATerminal` passes; the allow-list did not grow).
- [ ] **3. Security and privacy.** No secret or personal data in logs, the audit log, errors or docs; every new guard has a test that fails when the guard is removed (break-checked).
- [ ] **4. Intuitive over pretty (reviewer).** Reviewer: a first-time user finds the next step without reading; plain words, one obvious action per screen, no decoration that costs clarity.
- [ ] **5. Fewest actions.** Several requests of one agent are one approval (a batch with items); no flow asks the user twice for what one answer could cover.

## Checks

- [ ] `gofmt -l .` is empty, `go vet ./...` and `go test -race ./...` pass
