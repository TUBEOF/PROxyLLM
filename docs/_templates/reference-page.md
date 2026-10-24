# Reference page template

A reference page answers **what exists, precisely** — its reader arrives with
a question and leaves with a fact. Existing examples: `docs/configuration.md`,
`docs/reference/route-trace.md`, `docs/reference/model-compat.md`, every page
under `docs/api/`. A reference page is NOT followed top to bottom; every
section must stand alone and must be findable from `docs/README.md`'s index.

Delete this header and the section skeletons you do not fill.

## <Title>

One paragraph: the exact subject this page is the home of. State the
boundaries explicitly — what this page does NOT cover and where that lives
("the console pages are documented in `operating.md`"). One fact is defined
on exactly one page; every other page links here.

## <Thing>

Per entry: a table or a definition, never prose that a reader must parse.

| Key / Field / Setting | Type | Default | Meaning |
|---|---|---|---|
| `the_key` | type | default | one line, unambiguous; link deeper detail to its own section |

Rules that hold for the whole page (layers, precedence, encoding) come
BEFORE the tables, so a reader who only sees one table still gets them.

## <Sub-thing>

`##` for anything a reader might link to directly. Link-worthy facts get
their own heading; use anchors (`routing-and-fallbacks.md#this-thing`).

## Errors / Edge cases

The shapes and conditions a reader hits when things go wrong, each with the
behaviour it produces. Errors that belong to an API surface live in
`api/errors.md` — link, do not copy.

## See also

- Links to the ADR that explains *why*, and to the task page that uses this
  reference. Links only — this section is the page's hook into the map.