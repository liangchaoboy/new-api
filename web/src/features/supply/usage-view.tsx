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
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { toIntlLocale } from '@/i18n/languages'
import { formatNumber } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { supplyRequest, type MySupplyUsage } from './api'

function utcDate(timestamp: number) {
  return new Date(timestamp).toISOString().slice(0, 10)
}

export function SupplierUsageView(props: {
  summary?: boolean
  compact?: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const viewer = useAuthStore((s) => s.auth.user?.id)
  const today = utcDate(Date.now())
  const [from, setFrom] = useState(() => utcDate(Date.now() - 6 * 86400000))
  const [to, setTo] = useState(today)
  const [channel, setChannel] = useState('')
  const start = Date.parse(`${from}T00:00:00Z`) / 1000
  const end = Date.parse(`${to}T00:00:00Z`) / 1000
  const valid = end >= start && end - start < 31 * 86400 && to <= today
  const usage = useQuery({
    queryKey: ['supply', 'my-usage', viewer, start, end],
    queryFn: () =>
      supplyRequest<MySupplyUsage>(`usage?from=${start}&to=${end}`),
    enabled: valid && viewer != null,
    refetchInterval: 60_000,
  })
  const channels = new Map(
    usage.data?.rows.map((row) => [row.resource_id, row.channel])
  )
  const selected = channels.has(channel) ? channel : ''
  const rows = (usage.data?.rows ?? [])
    .filter((row) => !selected || row.resource_id === selected)
    .sort((a, b) => b.day - a.day || a.channel.localeCompare(b.channel, locale))
  const totals = rows.reduce(
    (total, row) => {
      total.records += row.records
      total.input += row.input_tokens
      total.output += row.output_tokens
      return total
    },
    { records: 0, input: 0, output: 0 }
  )
  const minutes = ((usage.data?.through ?? start) - start) / 60
  const rateFormat = new Intl.NumberFormat(locale, { maximumFractionDigits: 5 })
  const noData = t('No data')
  const rate = (value: number) =>
    usage.data?.complete && minutes > 0
      ? rateFormat.format(value / minutes)
      : noData
  const cacheTotal = (field: 'cache_read_tokens' | 'cache_write_tokens') => {
    const consumed = rows.filter((row) => row.records > 0)
    if (
      !consumed.length ||
      !usage.data?.complete ||
      consumed.some((row) => row[field] == null)
    ) {
      return noData
    }
    return formatNumber(
      consumed.reduce((total, row) => total + (row[field] ?? 0), 0),
      locale
    )
  }
  const cards = [
    { label: t('Records'), value: formatNumber(totals.records, locale) },
    { label: t('Input tokens'), value: formatNumber(totals.input, locale) },
    { label: t('Output tokens'), value: formatNumber(totals.output, locale) },
    {
      label: t('Total Tokens'),
      value: formatNumber(totals.input + totals.output, locale),
    },
    { label: t('Cache Read'), value: cacheTotal('cache_read_tokens') },
    { label: t('Cache Write'), value: cacheTotal('cache_write_tokens') },
    { label: t('Average RPM'), value: rate(totals.records) },
    { label: t('Average TPM'), value: rate(totals.input + totals.output) },
  ]
  const showSummary = usage.data && (usage.data.complete || rows.length > 0)
  const summaryCards = showSummary && (
    <div className='grid grid-cols-2 gap-3 lg:grid-cols-4'>
      {cards.map((card) => (
        <Card key={card.label} size='sm'>
          <CardHeader>
            <CardTitle>{card.label}</CardTitle>
          </CardHeader>
          <CardContent className='text-2xl font-semibold tabular-nums'>
            {card.value}
          </CardContent>
        </Card>
      ))}
    </div>
  )
  if (props.compact) {
    if (!usage.data?.has_windows) return null
    return (
      <section className='grid gap-2'>
        <div className='flex flex-wrap items-baseline justify-between gap-2'>
          <h3 className='font-semibold'>{t('Supplied channel usage')}</h3>
          <span className='text-muted-foreground text-sm'>
            {from} – {to} (UTC)
          </span>
        </div>
        {!usage.data.complete && (
          <p role='status'>{t('Usage data is incomplete')}</p>
        )}
        {summaryCards}
      </section>
    )
  }
  return (
    <section className='grid gap-4'>
      <div className='flex flex-wrap items-end gap-3'>
        <Label className='grid gap-2'>
          {t('Start date')} (UTC)
          <Input
            type='date'
            value={from}
            max={to}
            onChange={(e) => setFrom(e.target.value)}
          />
        </Label>
        <Label className='grid gap-2'>
          {t('End date')} (UTC)
          <Input
            type='date'
            value={to}
            min={from}
            max={today}
            onChange={(e) => setTo(e.target.value)}
          />
        </Label>
        {channels.size > 1 && (
          <Label className='grid gap-2'>
            {t('Channel')}
            <NativeSelect
              value={selected}
              onChange={(e) => setChannel(e.target.value)}
            >
              <NativeSelectOption value=''>
                {t('All channels')}
              </NativeSelectOption>
              {Array.from(channels, ([id, name]) => (
                <NativeSelectOption key={id} value={id}>
                  {name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </Label>
        )}
        <Button
          variant='outline'
          disabled={!valid || usage.isFetching}
          onClick={() => void usage.refetch()}
        >
          {t('Refresh')}
        </Button>
      </div>
      {!valid && <p role='status'>{t('Select up to 31 UTC days')}</p>}
      {valid && usage.isError && <p role='alert'>{t('Usage unavailable')}</p>}
      {valid && usage.data && !usage.data.complete && (
        <p role='status'>{t('Usage data is incomplete')}</p>
      )}
      {valid && !!usage.data?.as_of && (
        <p className='text-muted-foreground text-sm'>
          {t('Last updated')} ·{' '}
          {new Date(usage.data.as_of * 1000).toLocaleString(locale)}
        </p>
      )}
      {valid && props.summary && summaryCards}
      {!props.summary && (
        <p className='text-muted-foreground text-sm'>
          {t('Daily totals for your supplied channels.')}
        </p>
      )}
      {valid && !usage.isError && (
        <StaticDataTable
          data={rows}
          emptyContent={usage.isPending ? t('Loading...') : t('No data')}
          columns={[
            {
              id: 'day',
              header: `${t('Day')} (UTC)`,
              cell: (r) => utcDate(r.day * 1000),
            },
            { id: 'channel', header: t('Channel'), cell: (r) => r.channel },
            {
              id: 'model',
              header: t('Model'),
              cell: (r) => r.model || t('All models'),
            },
            {
              id: 'records',
              header: t('Records'),
              cell: (r) => formatNumber(r.records, locale),
            },
            {
              id: 'input',
              header: t('Input tokens'),
              cell: (r) => formatNumber(r.input_tokens, locale),
            },
            {
              id: 'output',
              header: t('Output tokens'),
              cell: (r) => formatNumber(r.output_tokens, locale),
            },
            {
              id: 'cache-read',
              header: t('Cache Read'),
              cell: (r) =>
                r.cache_read_tokens == null
                  ? noData
                  : formatNumber(r.cache_read_tokens, locale),
            },
            {
              id: 'cache-write',
              header: t('Cache Write'),
              cell: (r) =>
                r.cache_write_tokens == null
                  ? noData
                  : formatNumber(r.cache_write_tokens, locale),
            },
            {
              id: 'quota',
              header: t('Used Quota'),
              cell: (r) => formatNumber(r.quota, locale),
            },
          ]}
        />
      )}
    </section>
  )
}
