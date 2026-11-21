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
  useTranslation: () => ({ t: (key: string) => key }),
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

it('shows reports and hides channel changes for a read-only admin viewing another supplier', async () => {
  const { SupplyPage } = await import('../index')
  useAuthStore
    .getState()
    .auth.setUser({
      id: 8,
      username: 'reader',
      role: 10,
      permissions: { admin_permissions: { channel: { read: true } } },
    })
  try {
    vi.mocked(supplyRequest).mockImplementation(async (path) =>
      path === 'targets'
        ? []
        : [
            {
              id: 'c',
              name: 'Supplier C',
              supplier_id: 9,
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
    for (const name of [
      'Edit',
      'Assign channel',
      'Withdraw',
      'Delete',
      'Downstream instances',
    ]) {
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    }
  } finally {
    useAuthStore.getState().auth.setUser(null)
  }
})
