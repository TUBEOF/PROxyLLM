import { useMemo } from 'react';
import { apiCall } from '../api';
import type { ModelsResponse } from '../types';
import { CONFIG_LIVE_MS, PageHead, useLiveData } from '../hooks';
import ConnectSnippets from '../components/ConnectSnippets';

// /connect (ADR-026): one place with the exact configuration for each
// supported client. The key is a placeholder here — the real secret is shown
// exactly once, in the Keys dialog that creates it.
export default function Connect() {
  const { data } = useLiveData<ModelsResponse>(
    (signal) => apiCall<ModelsResponse>('GET', '/api/admin/models', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const models = useMemo(() => (data?.models ?? []).map((m) => m.id), [data]);

  return (
    <>
      <PageHead
        title="Connect a CLI"
        sub="Point Claude Code, Codex, OpenCode or any OpenAI-compatible client at this proxy. Create a key on the API keys page first — the secret is shown once."
      />
      <section className="widget">
        <div className="widget-body">
          <ConnectSnippets baseUrl={window.location.origin} apiKey="pk_..." models={models} />
        </div>
      </section>
    </>
  );
}
