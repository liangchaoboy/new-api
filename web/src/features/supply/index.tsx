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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { hasPermission } from '@/lib/admin-permissions'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { supplyRequest, type SupplyResource, type SupplyTarget } from './api'
import { DirectDeliveryDialog } from './delivery'
import { AssignmentEditor, ResourceEditor, TargetEditor } from './editor'
import { SupplyReport } from './report'

type View = {
  kind:
    | 'delivery'
    | 'confirm-revocation'
    | 'edit'
    | 'create'
    | 'targets'
    | 'assign'
    | 'archive'
    | 'withdraw'
    | 'usage'
    | 'history'
  resource?: SupplyResource
}

export function SupplyPage() {
  const { t } = useTranslation()
  const user = useAuthStore((s) => s.auth.user)
  const admin = (user?.role ?? 0) >= ROLE.ADMIN
  const canRead = admin && hasPermission(user, 'channel', 'read')
  const canWrite = admin && hasPermission(user, 'channel', 'write')
  const canOperate = admin && hasPermission(user, 'channel', 'operate')
  const sensitive = admin && hasPermission(user, 'channel', 'sensitive_write')
  const qc = useQueryClient()
  const [view, setView] = useState<View>()
  const resources = useQuery({
    queryKey: ['supply', 'resources', user?.id],
    queryFn: () => supplyRequest<SupplyResource[]>('resources'),
    refetchInterval: 5000,
  })
  const targets = useQuery({
    queryKey: ['supply', 'targets', user?.id],
    queryFn: () => supplyRequest<SupplyTarget[]>('targets'),
    enabled: canRead,
  })
  const close = () => setView(undefined)
  const saved = () => {
    close()
    void qc.invalidateQueries({ queryKey: ['supply'] })
  }
  const retry = useMutation({
    mutationFn: (id: string) => supplyRequest(`resources/${id}/retry`, 'POST'),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ['supply', 'resources'] }),
  })
  const withdraw = useMutation({
    mutationFn: (v: { kind: string; resource: SupplyResource }) => {
      const r = v.resource
      const archive = v.kind === 'archive'
      return supplyRequest(
        `resources/${r.id}${archive ? '' : '/withdraw'}?revision=${r.revision}`,
        archive ? 'DELETE' : 'POST'
      )
    },
    onSuccess: saved,
  })
  const revoke = useMutation({
    mutationFn: (r: SupplyResource) =>
      supplyRequest(`resources/${r.id}/revocation`, 'POST', {
        revision: r.revision,
        assignment: r.active_id,
        upstream_key_revoked: true,
      }),
    onSuccess: saved,
  })
  const targetLabel = (id: string, uid: number) => {
    const target = targets.data?.find((v) => v.id === id)
    const name = target?.mode === 'direct' ? target.url : (target?.name ?? id)
    return uid ? `${name} / ${uid}` : name
  }
  const r = view?.resource
  return (
    <>
      <SectionPageLayout>
        <SectionPageLayout.Title>
          {t('Supplier channels')}
        </SectionPageLayout.Title>
        <SectionPageLayout.Actions>
          {sensitive && canRead && (
            <Button
              variant='outline'
              onClick={() => setView({ kind: 'targets' })}
            >
              {t('Downstream instances')}
            </Button>
          )}
          <Button onClick={() => setView({ kind: 'create' })}>
            {t('Add channel')}
          </Button>
        </SectionPageLayout.Actions>
        <SectionPageLayout.Content>
          <StaticDataTable
            data={resources.data ?? []}
            getRowKey={(x) => x.id}
            emptyContent={resources.isLoading ? t('Loading...') : t('No data')}
            columns={[
              { id: 'name', header: t('Name'), cell: (x) => x.name },
              {
                id: 'owner',
                header: t('Supplier'),
                cell: (x) => x.supplier_id,
              },
              { id: 'models', header: t('Models'), cell: (x) => x.models },
              {
                id: 'user',
                header: t('Target'),
                cell: (x) =>
                  x.target ? targetLabel(x.target, x.user_id) : '—',
              },
              {
                id: 'effective',
                header: t('Confirmed target'),
                cell: (x) =>
                  x.effective
                    ? targetLabel(x.effective.target, x.effective.user_id)
                    : '—',
              },
              {
                id: 'version',
                header: t('Version'),
                cell: (x) => `${x.applied} / ${x.revision}`,
              },
              {
                id: 'status',
                header: t('Status'),
                cell: (x) => {
                  if (x.direct_state === 'await_revocation') {
                    return t('Awaiting upstream revocation')
                  }
                  if (x.direct_state === 'delivered') {
                    return t('Credentials exported')
                  }
                  if (x.direct_state === 'await_delivery' && !x.last_error) {
                    return t('Ready for handoff')
                  }
                  if (x.last_error) {
                    return `${t('Sync failed')} · ${t(x.last_error)}`
                  }
                  if (x.applied !== x.revision) return t('Pending')
                  if (x.archived) return t('Archived')
                  return x.target ? t('Applied') : t('Unassigned')
                },
              },
              {
                id: 'actions',
                header: t('Actions'),
                cell: (x) => (
                  <div className='flex flex-wrap gap-1'>
                    {user?.role === ROLE.SUPER_ADMIN &&
                      (x.direct_state === 'await_delivery' ||
                        x.direct_state === 'delivered') && (
                        <Button
                          variant='outline'
                          size='sm'
                          onClick={() =>
                            setView({ kind: 'delivery', resource: x })
                          }
                        >
                          {t('Export credentials')}
                        </Button>
                      )}
                    {x.direct_state === 'await_revocation' &&
                      (sensitive || x.supplier_id === user?.id) && (
                        <Button
                          variant='outline'
                          size='sm'
                          onClick={() =>
                            setView({ kind: 'confirm-revocation', resource: x })
                          }
                        >
                          {t('Confirm upstream revocation')}
                        </Button>
                      )}
                    {canWrite && x.last_error && (
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={retry.isPending}
                        onClick={() => retry.mutate(x.id)}
                      >
                        {t('Retry')}
                      </Button>
                    )}
                    {!x.archived && (
                      <>
                        {(x.supplier_id === user?.id || canWrite) && (
                          <Button
                            size='sm'
                            variant='outline'
                            onClick={() =>
                              setView({ kind: 'edit', resource: x })
                            }
                          >
                            {t('Edit')}
                          </Button>
                        )}
                        {canWrite && canRead && (
                          <Button
                            size='sm'
                            variant='outline'
                            onClick={() =>
                              setView({ kind: 'assign', resource: x })
                            }
                          >
                            {t('Assign channel')}
                          </Button>
                        )}
                        {(x.supplier_id === user?.id || canOperate) &&
                          x.target && (
                            <Button
                              size='sm'
                              variant='outline'
                              onClick={() =>
                                setView({ kind: 'withdraw', resource: x })
                              }
                            >
                              {t('Withdraw')}
                            </Button>
                          )}
                        {(x.supplier_id === user?.id || sensitive) && (
                          <Button
                            size='sm'
                            variant='outline'
                            onClick={() =>
                              setView({ kind: 'archive', resource: x })
                            }
                          >
                            {t('Delete')}
                          </Button>
                        )}
                      </>
                    )}
                    <Button
                      size='sm'
                      variant='ghost'
                      onClick={() => setView({ kind: 'usage', resource: x })}
                    >
                      {t('Usage')}
                    </Button>
                    <Button
                      size='sm'
                      variant='ghost'
                      onClick={() => setView({ kind: 'history', resource: x })}
                    >
                      {t('Operation logs')}
                    </Button>
                  </div>
                ),
              },
            ]}
          />
        </SectionPageLayout.Content>
      </SectionPageLayout>
      {(view?.kind === 'create' || view?.kind === 'edit') && (
        <ResourceEditor
          restrictSensitive={!!r && r.supplier_id !== user?.id && !sensitive}
          resource={r}
          close={close}
          saved={saved}
        />
      )}
      {view?.kind === 'targets' && (
        <TargetEditor
          targets={targets.data ?? []}
          close={close}
          saved={saved}
        />
      )}
      {r && view?.kind === 'assign' && (
        <AssignmentEditor
          resource={r}
          targets={targets.data ?? []}
          close={close}
          saved={saved}
        />
      )}
      {r && (view?.kind === 'usage' || view?.kind === 'history') && (
        <SupplyReport
          resource={r}
          targets={targets.data ?? []}
          history={view.kind === 'history'}
          close={close}
        />
      )}
      {r && view?.kind === 'delivery' && (
        <DirectDeliveryDialog
          resource={r}
          targetName={targetLabel(r.target, 0)}
          close={close}
          onExport={() =>
            void qc.invalidateQueries({ queryKey: ['supply', 'resources'] })
          }
        />
      )}
      {r && view?.kind === 'confirm-revocation' && (
        <ConfirmDialog
          open
          onOpenChange={(v) => {
            if (!v) close()
          }}
          destructive
          title={t('Confirm upstream revocation')}
          desc={t(
            'Confirm that the supplier has disabled the previously exported key.'
          )}
          isLoading={revoke.isPending}
          handleConfirm={() => revoke.mutate(r)}
        />
      )}
      {r && (view?.kind === 'archive' || view?.kind === 'withdraw') && (
        <ConfirmDialog
          open
          onOpenChange={(v) => {
            if (!v) close()
          }}
          destructive
          title={view.kind === 'archive' ? t('Delete') : t('Withdraw')}
          desc={r.name}
          isLoading={withdraw.isPending}
          handleConfirm={() =>
            withdraw.mutate({ kind: view.kind, resource: r })
          }
        />
      )}
    </>
  )
}
