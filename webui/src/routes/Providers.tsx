import { useCallback, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { apiCall } from '../api';
import type { ProvidersResponse, ProviderTypesResponse } from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useLiveData } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';

// Providers (ADR-009): every supported provider type is a card — its
// accounts, its connection state, and a click into the per-type settings
// page. No JSON editing anywhere: the settings page renders forms from the
// catalog the server ships. The old table+textarea drawer is gone.

export default function Providers() {
  const navigate = useNavigate();
  const { data: catalog, loading, error, updatedAt } = useLiveData<ProviderTypesResponse>(
    (signal) => apiCall<ProviderTypesResponse>('GET', '/api/admin/provider-types', undefined, signal),
    CONFIG_LIVE_MS,
  );
  const { data: providersData } = useLiveData<ProvidersResponse>(
    (signal) => apiCall<ProvidersResponse>('GET', '/api/admin/providers', undefined, signal),
    CONFIG_LIVE_MS,
  );

  const counts = useMemo(() => {
    const byType = new Map<string, { total: number }>();
    for (const p of providersData?.providers ?? []) {
      const cur = byType.get(p.type) ?? { total: 0 };
      cur.total += 1;
      byType.set(p.type, cur);
    }
    return byType;
  }, [providersData]);

  const open = useCallback(
    (type: string) => navigate(`/providers/${encodeURIComponent(type)}`),
    [navigate],
  );

  if (error !== null) {
    return <ErrorNote message={error} />;
  }
  if (loading || catalog === null) {
    return <EmptyState title="Loading providers" body="Fetching the provider catalog." />;
  }

  return (
    <>
      <PageHead
        title="Providers"
        sub="Every supported provider type. Add accounts per type — routing spreads requests over them."
        actions={<LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />}
      />
      <div className="canvas">
        <div
          className="w-full"
          style={{ display: 'grid', gap: 16, gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))' }}
        >
          {catalog.types.map((t) => {
            const count = counts.get(t.type)?.total ?? 0;
            return (
              <button
                key={t.type}
                type="button"
                className="card"
                style={{
                  display: 'flex',
                  flexDirection: 'column',
                  gap: 10,
                  textAlign: 'left',
                  cursor: 'pointer',
                  padding: 'var(--sp-4, 16px)',
                }}
                onClick={() => open(t.type)}
              >
                <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
                  <strong style={{ fontSize: 'var(--fs-md, 15px)' }}>{t.label}</strong>
                  {count > 0 ? (
                    <span className="badge up">
                      <span className="b-dot" />
                      {count} {count === 1 ? 'account' : 'accounts'}
                    </span>
                  ) : (
                    <span className="badge paused">
                      <span className="b-dot" />
                      No accounts
                    </span>
                  )}
                </div>
                <p style={{ margin: 0, fontSize: 'var(--fs-sm)', opacity: 0.85 }}>{t.description}</p>
                <span className="mono" style={{ fontSize: 'var(--fs-xs, 12px)', opacity: 0.6 }}>
                  {t.auth === 'oauth' ? 'OAuth device sign-in' : 'API key'}
                </span>
              </button>
            );
          })}
        </div>
      </div>
    </>
  );
}