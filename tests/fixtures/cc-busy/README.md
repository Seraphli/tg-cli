# cc-busy classifier fixtures

`manifest.tsv` drives `TestCCBusyFromViewportFixtures` (cmd/helpers/cc_busy_test.go). It is pure
tab-separated data — one row per fixture, `<filename>` TAB `<verdict>`, where `<verdict>` is `busy`
or `idle`. Keeping it free of comment lines gives every row the same field count, so it renders as a
table in web git viewers. The test asserts `ccBusyFromViewport` returns the listed verdict for each
fixture and requires at least 17 data rows (a guard against a silently-empty fixture directory).

## Minimal-fixture standard

Each `*.txt` fixture keeps ONLY the content the classifier needs, never a whole-pane capture:

- the classifier box: the top and bottom rule borders, the label carried on the top border where
  that label is the tested feature, and the prompt line (with a neutral invented draft where a draft
  is the tested feature);
- the byte-exact discriminating rows: the spinner/status line, the done-line for idle fixtures, the
  right-aligned token counter, and the effort/chrome rows that sit between the status and the border;
- a few invented neutral filler lines above the box (plus a short blank gap where the case needs one);
- a genericized bottom chrome line, with real model names replaced by a neutral placeholder.

Everything outside those rows is unrelated pane content and is dropped when a fixture is built or
rebuilt. If a minimal rebuild changes a fixture's verdict, fix the fixture — never the test or the
manifest.

## No private or session content

Fixtures and this manifest must contain none of the following classes of private or session data:
account usernames, home or workspace directory paths, email addresses, API tokens or keys, IP
addresses, private hostnames or domains, chat or channel identifiers, project or agent names, and
verbatim transcript, essay, or shell-command content. A private-content scan across every fixture and
the manifest reports zero matches.
