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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it, vi } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { supplyRequest } from '../api'
import { ResourceEditor } from '../editor'

vi.mock('../api', () => ({ supplyRequest: vi.fn() }))
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key, i18n: { language: 'en' } }),
}))
it('preserves the stored credential when saving an edited resource with an empty key', async () => {
  const user = userEvent.setup()
  const saved = vi.fn()
  vi.mocked(supplyRequest).mockResolvedValue({ id: 'sample' })
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ResourceEditor
        resource={{
          id: 'sample',
          supplier_id: 1,
          type: 1,
          models: 'gpt-4o',
          name: 'Original',
          base_url: '',
          revision: 3,
          applied: 3,
          target: '',
          user_id: 0,
          last_error: '',
          archived: false,
        }}
        close={vi.fn()}
        saved={saved}
      />
    </QueryClientProvider>
  )
  const name = screen.getByRole('textbox', { name: 'Name' })
  await user.clear(name)
  await user.type(name, 'Renamed')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(saved).toHaveBeenCalledOnce())
  expect(supplyRequest).toHaveBeenCalledWith(
    'resources/sample',
    'PUT',
    expect.objectContaining({ name: 'Renamed', key: '', revision: 3 })
  )
})

it('opens the creation form from the supplier channel list', async () => {
  const { SupplyPage } = await import('../index')
  vi.mocked(supplyRequest).mockResolvedValue([])
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={new QueryClient()}>
      <SupplyPage />
    </QueryClientProvider>
  )
  await user.click(screen.getByRole('button', { name: 'Add channel' }))
  expect(
    await screen.findByRole('dialog', { name: 'Add channel' })
  ).toBeInTheDocument()
})

it('keeps a restricted admin from viewing downstream identities or assigning channels', async () => {
  const { SupplyPage } = await import('../index')
  useAuthStore.getState().auth.setUser({
    id: 8,
    username: 'reader',
    role: 10,
    permissions: {
      admin_permissions: { channel: { read: true, write: true } },
    },
  })
  try {
    vi.mocked(supplyRequest).mockClear()
    vi.mocked(supplyRequest).mockImplementation(async (path) =>
      path === 'targets'
        ? []
        : [
            {
              id: 'c',
              name: 'Supplier C',
              supplier_id: 8,
              assigned: true,
              target: 'private-downstream',
              effective: {
                target: 'private-downstream',
                user_id: 12,
                channel_id: 34,
              },
              revision: 1,
              applied: 1,
            },
          ]
    )
    render(
      <QueryClientProvider client={new QueryClient()}>
        <SupplyPage />
      </QueryClientProvider>
    )
    expect(await screen.findByText('Supplier C')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Usage' })).toBeInTheDocument()
    expect(screen.getByText('Applied')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Edit' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Withdraw' })).toBeInTheDocument()
    expect(
      screen.queryByRole('columnheader', { name: 'Confirmed target' })
    ).not.toBeInTheDocument()
    expect(screen.queryByText('private-downstream')).not.toBeInTheDocument()
    expect(supplyRequest).not.toHaveBeenCalledWith('targets')
    for (const name of ['Assign channel', 'Downstream instances']) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
  } finally {
    useAuthStore.getState().auth.setUser(null)
  }
})

it('registers a direct instance using its URL and ordinary account without asking for a token', async () => {
  const { TargetEditor } = await import('../editor')
  const user = userEvent.setup()
  const saved = vi.fn()
  vi.mocked(supplyRequest).mockResolvedValue({ id: 'target' })
  render(
    <QueryClientProvider client={new QueryClient()}>
      <TargetEditor targets={[]} close={vi.fn()} saved={saved} />
    </QueryClientProvider>
  )
  await user.type(
    screen.getByRole('textbox', { name: 'Base URL' }),
    'https://customer.example'
  )
  await user.type(screen.getByRole('textbox', { name: 'Username' }), 'partner')
  await user.type(
    screen.getByLabelText('Password', { selector: 'input' }),
    'fixture-only'
  )
  expect(screen.queryByLabelText('Access token')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(saved).toHaveBeenCalledOnce())
  expect(supplyRequest).toHaveBeenCalledWith('targets', 'POST', {
    url: 'https://customer.example',
    username: 'partner',
    password: 'fixture-only',
    access_method: 'saved_account',
  })
})

it.each(['customer_account', 'direct'])(
  'assigns a %s customer instance without a downstream UID',
  async (mode) => {
    const { AssignmentEditor } = await import('../editor')
    const user = userEvent.setup()
    const saved = vi.fn()
    vi.mocked(supplyRequest).mockResolvedValue(null)
    render(
      <QueryClientProvider client={new QueryClient()}>
        <AssignmentEditor
          targets={[
            {
              id: 'target',
              name: 'customer',
              url: 'https://customer.example',
              mode,
              account_api: 'official_channels',
              usage_api: 'channel_logs',
            },
          ]}
          resource={{
            id: 'c',
            supplier_id: 1,
            name: 'Channel',
            models: 'm',
            type: 1,
            base_url: '',
            target: '',
            user_id: 0,
            revision: 1,
            applied: 1,
            archived: false,
            last_error: '',
          }}
          close={vi.fn()}
          saved={saved}
        />
      </QueryClientProvider>
    )
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Target' }),
      'target'
    )
    if (mode === 'customer_account') {
      expect(screen.getByLabelText('Group')).toHaveValue('default')
    }
    expect(
      screen.queryByRole('spinbutton', { name: 'User ID' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(saved).toHaveBeenCalledOnce())
    expect(supplyRequest).toHaveBeenCalledWith(
      'resources/c/assignment',
      'PUT',
      {
        target: 'target',
        user_id: 0,
        revision: 1,
        scope: 'instance',
        customer_group: mode === 'customer_account' ? 'default' : undefined,
      }
    )
  }
)

it('shows averages for synced minutes and distinguishes zero cache from an omitted field', async () => {
  const { SupplierUsageView } = await import('../usage-view')
  useAuthStore.getState().auth.setUser({ id: 8, username: 'supplier', role: 1 })
  try {
    vi.mocked(supplyRequest).mockImplementation(async (path) => {
      const start = Number(
        new URL(path, 'https://fixture.invalid/').searchParams.get('from')
      )
      return {
        as_of: start + 100 * 60,
        through: start + 100 * 60,
        has_windows: true,
        complete: true,
        rows: [
          {
            resource_id: 'c',
            channel: 'fixture',
            day: start,
            model: 'demo',
            records: 5,
            input_tokens: 59,
            output_tokens: 800,
            cache_read_tokens: 0,
          },
        ],
      }
    })
    render(
      <QueryClientProvider client={new QueryClient()}>
        <SupplierUsageView summary compact />
      </QueryClientProvider>
    )
    const card = (title: string) =>
      screen.getByText(title).closest('[data-slot="card"]')
    await screen.findByText('Average RPM')
    expect(card('Average RPM')).toHaveTextContent('0.05')
    expect(card('Average TPM')).toHaveTextContent('8.59')
    expect(card('Cache Read')).toHaveTextContent('0')
    expect(card('Cache Write')).toHaveTextContent('No data')
  } finally {
    useAuthStore.getState().auth.setUser(null)
  }
})
