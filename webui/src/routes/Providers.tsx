import { useMemo } from 'react';
import { Link } from 'react-router-dom';
import { apiCall } from '../api';
import type { ProvidersResponse, ProviderTypesResponse } from '../types';
import { CONFIG_LIVE_MS, EmptyState, ErrorNote, PageHead, useLiveData } from '../hooks';
import { LiveBadge } from '../components/LiveBadge';

// Providers (ADR-009): every supported provider type is a card — its
// accounts, its connection state, and a click into the per-type settings
// page. No JSON editing anywhere: the settings page renders forms from the
// catalog the server ships. The old table+textarea drawer is gone.

export default function Providers() {
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

  const head = (
    <PageHead
      title="Providers"
      sub="Every supported provider type. Add accounts per type — routing spreads requests over them."
      actions={
        <>
          <LiveBadge updatedAt={updatedAt} intervalMs={CONFIG_LIVE_MS} />
          <Link className="btn btn-ghost btn-sm" to="/setup">
            Setup wizard
          </Link>
        </>
      }
    />
  );
  if (error !== null) {
    return (
      <>
        {head}
        <div className="canvas">
          <div className="w-full"><ErrorNote message={error} /></div>
        </div>
      </>
    );
  }
  if (loading || catalog === null) {
    return (
      <>
        {head}
        <div className="canvas">
          <div className="w-full">
            <EmptyState title="Loading providers" body="Fetching the provider catalog." />
          </div>
        </div>
      </>
    );
  }

  return (
    <>
      {head}
      <div className="canvas">
        <div
          className="w-full"
          style={{ display: 'grid', gap: 16, gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))' }}
        >
          {catalog.types.map((t) => {
            const count = counts.get(t.type)?.total ?? 0;
            return (
              <Link
                key={t.type}
                to={`/providers/${encodeURIComponent(t.type)}`}
                className="widget provider-type-card"
              >
                <div className="type-head">
                  <h3 className="title">{t.label}</h3>
                  {count > 0 ? (
                    <span className="badge up type-count">
                      <span className="b-dot" />
                      {count} {count === 1 ? 'account' : 'accounts'}
                    </span>
                  ) : (
                    <span className="badge paused type-count">
                      <span className="b-dot" />
                      No accounts
                    </span>
                  )}
                </div>
                <p className="type-desc">{t.description}</p>
                <span className="mono hint type-auth">
                  {t.auth === 'oauth' ? 'OAuth device sign-in' : 'API key'}
                </span>
              </Link>
            );
          })}
        </div>
      </div>
    </>
  );
}