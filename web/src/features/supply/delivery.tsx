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
import { useState, useEffect, useRef } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  SecureVerificationDialog,
  useSecureVerification,
} from '@/features/auth/secure-verification'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'

import type { SupplyResource } from './api'

type Credentials = {
  name: string
  base_url: string
  models: string
  type: number
  key: string
}

export function DirectDeliveryDialog(props: {
  resource: SupplyResource
  targetName: string
  close: () => void
  onExport: () => void
}) {
  const { t } = useTranslation()
  const verification = useSecureVerification()
  const cancel = verification.cancel
  const [result, setResult] = useState<Credentials | null>(null)
  const [loading, setLoading] = useState(false)
  const pending = useRef<AbortController | null>(null)
  const r = props.resource
  useEffect(
    () => () => {
      pending.current?.abort()
      cancel()
    },
    [cancel]
  )
  async function reveal() {
    if (pending.current) return
    const op = new AbortController()
    pending.current = op
    setLoading(true)
    try {
      const context = {
        resource_id: r.id,
        target: r.target,
        revision: r.revision,
      }
      const proof = await verification.requestVerification({
        scope: 'supply.key.deliver',
        context,
        title: t('Export credentials'),
        description: t('Verify to view channel key'),
      })
      if (!proof || op.signal.aborted) return
      const res = await api.post<{ success: boolean; data: Credentials }>(
        `/api/supply/resources/${r.id}/delivery`,
        { revision: r.revision, target: r.target },
        {
          headers: { 'X-Security-Proof': proof.proof_token },
          signal: op.signal,
        }
      )
      if (!op.signal.aborted) {
        setResult(requireServerSuccess(res.data).data)
        props.onExport()
      }
    } catch (err) {
      if (!op.signal.aborted) handleServerError(err)
    } finally {
      pending.current = null
      if (!op.signal.aborted) setLoading(false)
    }
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open) props.close()
        }}
        title={t('Export credentials')}
        description={props.targetName}
      >
        <p>
          {t(
            'Give the upstream URL, models and dedicated key to the customer administrator.'
          )}
        </p>
        {result ? (
          <div className='grid gap-2'>
            <div className='flex items-center gap-2'>
              {result.base_url}
              <CopyButton value={result.base_url} tooltip={t('Copy URL')} />
            </div>
            <div>{result.models}</div>
            <CopyButton value={result.key} variant='outline' size='default'>
              {t('Copy API key')}
            </CopyButton>
            <CopyButton
              value={JSON.stringify(result, null, 2)}
              size='default'
              variant='outline'
            >
              {t('Configuration')}
            </CopyButton>
          </div>
        ) : (
          <Button disabled={loading} onClick={() => void reveal()}>
            {t('Verify to view channel key')}
          </Button>
        )}
      </Dialog>
      <SecureVerificationDialog {...verification.dialogProps} />
    </>
  )
}
