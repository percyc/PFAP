# Lab screenshot provenance

Captured on 2026-09-12 from the authenticated local PFAP Lab controller using
headless Chromium at a 1440 × 1000 viewport, device scale factor 1.
The repository was at `f466691` before this documentation-only update.

These are actual UI screenshots, not mockups or substituted fixtures:

| File | Source and scope |
| --- | --- |
| `run-report.png` | First completed run card in the selected experiment, with “查看窗口报告” expanded; the card is captured directly as a browser element |
| `experiments.png` | Experiment tab filtered by the selected experiment ID; viewport cropped before unused lower space |
| `overview.png` | Experiment-scoped overview; viewport cropped after the historical metric cards, before operational event details |

Experiment: `exp-c17eebe55019`.
Run: `load-6f04e52c6b45` (September 11, 2026, 08:51:12–09:51:12 UTC+8).
The experiment was already stopped, and all 156 formal submissions had confirmed.
The run card's cumulative 241 also includes 85 warmup submissions.

No values, labels, statuses or timestamps were rewritten. The overview's header
counts span saved experiments; its filtered metrics span the selected experiment's
history, not the single formal window. The figures must not be compared as if
they had identical scope. Browser-local display times may differ from host time.

## Refreshing the screenshots safely

1. Use an existing experiment with a verified completed report. Never start,
   restart or stop nodes just to produce documentation images.
2. Sign in locally without logging the password, cookie or full state snapshot.
3. After login, allow only GET/HEAD browser requests; abort mutation requests.
   Navigate by tabs, selectors and the report's read-only view button.
4. Capture the corresponding viewport regions or run-card element. Keep SSH
   addresses, credentials, private-account details and raw event/error bodies
   outside the screenshot. Do not replace metrics or hide failures to imply success.
5. Inspect every PNG visually, check browser errors and unexpected write attempts,
   update captions/date/source IDs here, and verify relative README image links.

This capture had zero browser page errors and zero attempted mutation requests
after login. Login created only a browser session; no deployment, transaction,
account or server configuration was changed. All three images were visually
reviewed before publication in the repository.

Old console GIFs in the parent directory are retained as historical assets but
are no longer embedded in the project or CLI guide. They are not current
reproduction instructions.
