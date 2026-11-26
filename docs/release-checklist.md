# v2.0.0 manual smoke checklist

Run after the PR chain is merged and before announcing the release. The
maintainer runs these; the implementation prepares them.

1. `curl http://<host>:8080/healthz` answers `ok`.
2. First visit bootstraps the admin; the wizard opens.
3. Wizard: add a provider, run the model test, create a key, reach
   **Connect a CLI**, finish.
4. Create an `anthropic-oauth` account: the risk notice appears, Cancel
   leaves the form closed, acknowledging opens it, the paste field is visible.
5. `/connect` renders all four tabs with the console's origin.
6. Install one CLI with `scripts/install.sh --yes`, send one message, and see
   the request in **Logs** with a `route_trace`.
7. `/v1/model_catalog` answers a top-level `models` array; Codex's `/model`
   picker shows a proxy model.
8. After the image is published: pull `ghcr.io/tubeof/proxyllm:v2.0.0`, then
   pin `docker/compose.yaml` and the deployment example to that tag.
9. Move `SECURITY.md`'s supported line to 2.x only and publish the release
   notes (strip the draft/checklist sections).
