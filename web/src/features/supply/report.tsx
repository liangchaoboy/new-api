/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import {
  supplyRequest,
  type SupplyEvent,
  type SupplyResource,
  type SupplyUsage,
  type SupplyTarget,
} from './api'

export function SupplyReport(props: {
  resource: SupplyResource
  history: boolean
  targets: SupplyTarget[]
  close: () => void
}) {
  const { t, i18n } = useTranslation()
  const viewer = useAuthStore((s) => s.auth.user?.id)
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const today = new Date().toISOString().slice(0, 10)
  const [from, setFrom] = useState(today),
    [to, setTo] = useState(today),
    [uid, setUid] = useState(''),
    [model, setModel] = useState(''),
    [target, setTarget] = useState('')
  const start = Date.parse(`${from}T00:00:00Z`) / 1000,
    end = Date.parse(`${to}T00:00:00Z`) / 1000
  const valid = end >= start && end - start < 31 * 86400 && to <= today
  const id = props.resource.id,
    base = `resources/${id}/`
  const params = new URLSearchParams({
    from: String(start),
    to: String(end),
    user_id: uid,
    model,
    target,
  })
  const usage = useQuery({
    queryKey: ['supply', 'usage', viewer, id, params.toString()],
    queryFn: () => supplyRequest<SupplyUsage>(`${base}usage?${params}`),
    enabled: !props.history && valid,
  })
  const history = useQuery({
    queryKey: ['supply', 'history', viewer, id],
    queryFn: () => supplyRequest<SupplyEvent[]>(`${base}history`),
    enabled: props.history,
  })
  const provider = useQuery({
    queryKey: ['supply', 'provider-usage', viewer, id],
    queryFn: () =>
      supplyRequest<
        {
          target: string
          total_used: number | null
          as_of: number
          current: boolean
        }[]
      >(`${base}provider-usage`),
    enabled: !props.history,
  })
  const showManaged =
    usage.data?.has_windows ||
    (!provider.data?.length && !props.resource.direct_state)
  const actionNames: Record<string, string> = {
    customer_channel_saved: t('Customer channel confirmed'),
    customer_channel_removed: t('Customer channel retired'),
    key_exported: t('Credentials exported'),
    revocation_confirmed: t('Upstream revocation confirmed'),
    created: t('Created'),
    updated: t('Updated'),
    archived: t('Archived'),
    withdrawn: t('Withdraw'),
    revoked: t('Revoked'),
    assignment_requested: t('Assign channel'),
    applied: t('Applied'),
    sync_failed: t('Sync failed'),
  }
  const rows = usage.data?.rows ?? [],
    targetName = (value: string) =>
      props.targets.find((x) => x.id === value)?.name || value || '—'
  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) props.close()
      }}
      title={`${props.resource.name} · ${
        props.history ? t('Operation logs') : t('Usage')
      }`}
      contentClassName='sm:max-w-4xl'
    >
      {props.history ? (
        <StaticDataTable
          data={history.data ?? []}
          getRowKey={(r) => r.id}
          emptyContent={history.isPending ? t('Loading...') : t('No data')}
          columns={[
            {
              id: 'time',
              header: t('Time'),
              cell: (r) => new Date(r.created * 1000).toLocaleString(locale),
            },
            {
              id: 'action',
              header: t('Action'),
              cell: (r) => actionNames[r.action] ?? r.action,
            },
            {
              id: 'actor',
              header: t('Operator'),
              cell: (r) => r.actor || t('System'),
            },
            {
              id: 'target',
              header: t('Target'),
              cell: (r) => targetName(r.target),
            },
            { id: 'user', header: t('User ID'), cell: (r) => r.user_id || '—' },
            { id: 'version', header: t('Version'), cell: (r) => r.revision },
          ]}
        />
      ) : (
        <div className='grid gap-4'>
          {!showManaged && !provider.data?.length && (
            <p>
              {provider.isPending ? t('Loading...') : t('Usage unavailable')}
            </p>
          )}
          {!!provider.data?.length && (
            <section className='grid gap-2'>
              <h3>{t('Provider key usage')}</h3>
              <p>
                {t(
                  'Lifetime quota reported by the provider for this dedicated key.'
                )}
              </p>
              <StaticDataTable
                data={provider.data}
                columns={[
                  {
                    id: 'target',
                    header: t('Target'),
                    cell: (r) => targetName(r.target),
                  },
                  {
                    id: 'quota',
                    header: t('Used Quota'),
                    cell: (r) =>
                      r.total_used === null
                        ? t('Usage unavailable')
                        : formatNumber(r.total_used, locale),
                  },
                  {
                    id: 'at',
                    header: t('Last updated'),
                    cell: (r) =>
                      r.as_of
                        ? new Date(r.as_of * 1000).toLocaleString(locale)
                        : '—',
                  },
                  {
                    id: 'status',
                    header: t('Status'),
                    cell: (r) => (r.current ? t('Current') : t('Last known')),
                  },
                ]}
              />
              <Button
                variant='outline'
                disabled={provider.isFetching}
                onClick={() => void provider.refetch()}
              >
                {t('Refresh')}
              </Button>
            </section>
          )}
          {showManaged && (
            <>
              <div className='grid gap-3 sm:grid-cols-3'>
                <Label>
                  {t('Start date')} (UTC)
                  <Input
                    type='date'
                    value={from}
                    max={to}
                    onChange={(e) => setFrom(e.target.value)}
                  />
                </Label>
                <Label>
                  {t('End date')} (UTC)
                  <Input
                    type='date'
                    value={to}
                    min={from}
                    max={today}
                    onChange={(e) => setTo(e.target.value)}
                  />
                </Label>
                <Label>
                  {t('User ID')}
                  <Input
                    type='number'
                    min='1'
                    value={uid}
                    onChange={(e) => setUid(e.target.value)}
                  />
                </Label>
                <Label>
                  {t('Model')}
                  <Input
                    value={model}
                    onChange={(e) => setModel(e.target.value)}
                  />
                </Label>
                <Label>
                  {t('Target')}
                  <Input
                    list='supply-report-targets'
                    value={target}
                    onChange={(e) => setTarget(e.target.value)}
                  />
                  <datalist id='supply-report-targets'>
                    {props.targets.map((x) => (
                      <option key={x.id} value={x.id}>
                        {x.name}
                      </option>
                    ))}
                  </datalist>
                </Label>
                <Button
                  disabled={!valid || usage.isFetching}
                  onClick={() => void usage.refetch()}
                >
                  {t('Refresh')}
                </Button>
              </div>
              {!valid && <p>{t('Select up to 31 UTC days')}</p>}
              {usage.data && !usage.data.complete && (
                <p role='status'>{t('Usage data is incomplete')}</p>
              )}
              {usage.data && usage.data.as_of > 0 && (
                <p>
                  {t('Last updated')} ·{' '}
                  {new Date(usage.data.as_of * 1000).toLocaleString(locale)}
                </p>
              )}
              {usage.data && (usage.data.complete || rows.length > 0) && (
                <p>
                  {t('Total')} · {t('Records')}{' '}
                  {formatNumber(
                    rows.reduce((n, r) => n + r.records, 0),
                    locale
                  )}{' '}
                  · {t('Input tokens')}{' '}
                  {formatNumber(
                    rows.reduce((n, r) => n + r.input_tokens, 0),
                    locale
                  )}{' '}
                  · {t('Output tokens')}{' '}
                  {formatNumber(
                    rows.reduce((n, r) => n + r.output_tokens, 0),
                    locale
                  )}
                </p>
              )}
              <StaticDataTable
                data={rows}
                emptyContent={usage.isFetching ? t('Loading...') : t('No data')}
                columns={[
                  {
                    id: 'day',
                    header: 'UTC',
                    cell: (r) =>
                      new Date(r.day * 1000).toISOString().slice(0, 10),
                  },
                  {
                    id: 'target',
                    header: t('Target'),
                    cell: (r) => targetName(r.target),
                  },
                  {
                    id: 'uid',
                    header: t('User ID'),
                    cell: (r) => r.user_id || '—',
                  },
                  { id: 'model', header: t('Model'), cell: (r) => r.model },
                  {
                    id: 'records',
                    header: t('Records'),
                    cell: (r) => formatNumber(r.records, locale),
                  },
                  {
                    id: 'channel',
                    header: t('Channel ID'),
                    cell: (r) => formatNumber(r.channel_id, locale),
                  },
                  {
                    id: 'quota',
                    header: t('Used Quota'),
                    cell: (r) =>
                      r.quota == null ? '—' : formatNumber(r.quota, locale),
                  },
                  {
                    id: 'in',
                    header: t('Input tokens'),
                    cell: (r) => formatNumber(r.input_tokens, locale),
                  },
                  {
                    id: 'out',
                    header: t('Output tokens'),
                    cell: (r) => formatNumber(r.output_tokens, locale),
                  },
                ]}
              />
            </>
          )}
        </div>
      )}
    </Dialog>
  )
}
