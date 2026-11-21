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
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

export type SupplyResource = {
  id: string
  supplier_id: number
  name: string
  models: string
  type: number
  base_url: string
  target: string
  user_id: number
  revision: number
  applied: number
  archived: boolean
  effective?: { target: string; user_id: number; channel_id: number }
  last_error: string
}
export type SupplyTarget = { id: string; name: string; url: string }
export type SupplyEvent = {
  id: number
  created: number
  action: string
  actor: number
  target: string
  user_id: number
  revision: number
}
export type SupplyUsage = {
  as_of: number
  complete: boolean
  rows: {
    day: number
    target: string
    user_id: number
    model: string
    records: number
    input_tokens: number
    output_tokens: number
  }[]
}

export async function supplyRequest<T>(
  path: string,
  method = 'GET',
  data?: unknown
): Promise<T> {
  const res = await api.request<{ success: boolean; data: T }>({
    url: `/api/supply/${path}`,
    method,
    data,
  })
  return requireServerSuccess(res.data).data
}
