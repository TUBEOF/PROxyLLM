# PROxyLLM documentation

Four ways in, depending on what you are doing.

## Try it

| Question | Page |
|---|---|
| How do I get it running and send a first request? | [Getting started](getting-started.md) |
| How do I deploy this properly — container, TLS, sizing, backups? | [Deployment](deployment.md) |

## Run it

| Question | Page |
|---|---|
| Which settings exist, and which layer wins? | [Configuration](configuration.md) |
| What does each page of the console do? | [Operating PROxyLLM](operating.md) |
| How do I route, fail over, and why did *this* request land there? | [Routing and fallbacks](routing-and-fallbacks.md) |
| What is the security posture and the threat model? | [Security](security.md) |
| How do I report a vulnerability? | [SECURITY.md](../SECURITY.md) |

## Call it

| Question | Page |
|---|---|
| Which endpoints exist, how do I authenticate, what are the shared conventions? | [API reference](api/README.md) |
| What does the OpenAI-compatible surface accept? | [OpenAI-compatible](api/openai.md) |
| What does the Anthropic-compatible surface accept? | [Anthropic-compatible](api/anthropic.md) |
| How do decision models work? | [Decision API](api/decisions.md) |
| What does an error look like on each surface? | [Errors](api/errors.md) |
| How do I drive the operator API? | [Admin API](api/admin.md) |
| How do I run Claude Code against a non-Claude model? | [Claude Code](claude-code.md) |

`docs/api/openapi.json` is an OpenAPI 3.1 description of the client surfaces.

## Build it

| Question | Page |
|---|---|
| How is the system put together, and why is it built this way? | [Architecture decision records](adr/README.md) |
| How do I build, test, run and release it? | [Development](development.md) |
| How do I run the live CLI suite and see which CLIs currently work? | [Live CLI workflow](test-instance.md) |
| How do I contribute? | [CONTRIBUTING.md](../CONTRIBUTING.md) |
| How does field X of protocol A map to protocol B? | [Translation matrix](reference/translation-matrix.md) |
| What does a per-model compatibility setting do? | [Model compatibility](reference/model-compat.md) |
| What is in a `route_trace`, and which versions exist? | [Route trace](reference/route-trace.md) |
| How are wire facts captured and tested? | [Golden fixtures](../testdata/golden/README.md) |

## How these docs are organised

**A fact is defined on exactly one page.** Other pages link to it instead of
restating it, because a fact stated twice drifts once. The homes are: the
endpoints in `api/`, the compatibility settings in `reference/model-compat.md`,
the trace schema in `reference/route-trace.md`, the configuration keys in
`configuration.md`, the console behaviour in `operating.md`, the routing
semantics in `routing-and-fallbacks.md`, and the *why* in `adr/`.

The layers are:

- **Task pages** (`getting-started`, `deployment`, `operating`,
  `claude-code`) — what to do, in order.
- **Reference pages** (`api/`, `reference/`, `configuration`) — what exists,
  precisely.
- **`adr/`** — one file per architectural decision, with a stable `ADR-NNN`
  identifier the source code cites in its comments.
- **`design/`** — the console's design system: rules (`DESIGN.md`), the
  token source (`colors_and_type.css`), and the machine-readable profile.
- **`testdata/golden/`** — the wire fixtures, each with the URL it was read
  from and the date.

## Writing a page

Start from the template that matches the page's layer:

| Writing… | Template |
|---|---|
| A task page (what to do, in order) | [`_templates/task-page.md`](_templates/task-page.md) |
| A reference page (what exists, precisely) | [`_templates/reference-page.md`](_templates/reference-page.md) |
| An endpoint block in `api/` | [`_templates/api-endpoint.md`](_templates/api-endpoint.md) |
| An ADR | [`adr/TEMPLATE.md`](adr/TEMPLATE.md) |
| A wire fixture | [`../testdata/golden/_templates/fixture.meta.json`](../testdata/golden/_templates/fixture.meta.json) |

Templates carry the shape, not facts: fill them, then cut every section that
stayed empty. A new page lands in the index above in the same change, and
the page it takes a fact from loses that fact — stated twice, drifted once.
