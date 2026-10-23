# Task page template

A task page answers **what to do, in order** — it is written for someone
following it top to bottom, not for someone looking a fact up. Existing
examples: `docs/getting-started.md`, `docs/deployment.md`,
`docs/operating.md`, `docs/claude-code.md`.

Delete this header and the section skeletons you do not fill.

## <Title>

One paragraph: what the reader achieves by following the page, and the
state they need to start (installed, configured, signed in — with a link
to the page that gets them there).

## Prerequisites

- Bulleted, checkable facts. Each prerequisite links to the reference page
  that defines it. A task page never defines a setting here — it links to
  `configuration.md`.

## <Step 1 — imperative>

Ordered `##` sections, one per step, each starting with a verb.

1. Concrete instruction — a command in a fenced block when there is one.
2. What the result looks like, so the reader can verify the step.
3. What to do when it does not look like that (a pointer to the reference
   page or error page, not a copy of its content).

## <Step N — imperative>

Repeat. Keep each step to one screen. If two steps really belong together,
they are one step.

## Troubleshooting

| Symptom | Cause and where it is solved |
|---|---|
| Short, searchable symptom | Link to the reference/API/error page that owns the fact |

## Next steps

- Where a finished reader goes next — links only, one line each.