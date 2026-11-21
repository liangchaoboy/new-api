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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { PasswordInput } from '@/components/password-input'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'

import { supplyRequest, type SupplyResource, type SupplyTarget } from './api'

const schema = z.object({
  name: z.string().trim().min(1).max(128),
  type: z.string(),
  base_url: z.string().trim().max(512),
  models: z.string().trim().min(1),
  key: z.string().max(48000),
})
export function ResourceEditor(props: {
  restrictSensitive?: boolean
  resource?: SupplyResource
  close: () => void
  saved: () => void
}) {
  const { t } = useTranslation()
  const r = props.resource
  const form = useForm({
    resolver: zodResolver(schema),
    defaultValues: {
      name: r?.name ?? '',
      type: String(r?.type ?? 1),
      base_url: r?.base_url ?? '',
      models: r?.models ?? '',
      key: '',
    },
  })
  const save = useMutation({
    mutationFn: (v: z.infer<typeof schema>) =>
      supplyRequest(r ? `resources/${r.id}` : 'resources', r ? 'PUT' : 'POST', {
        ...v,
        type: Number(v.type),
        models: v.models
          .split(',')
          .map((x) => x.trim())
          .join(','),
        revision: r?.revision,
      }),
    onSuccess: props.saved,
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.close()
      }}
      title={r ? t('Edit channel') : t('Add channel')}
    >
      <form
        className='grid gap-4'
        onSubmit={form.handleSubmit((v) => save.mutate(v))}
      >
        <Label>
          {t('Name')}
          <Input required maxLength={128} {...form.register('name')} />
        </Label>
        <Label>
          {t('Type')}
          <NativeSelect
            disabled={props.restrictSensitive}
            {...form.register('type')}
          >
            {[
              [1, 'OpenAI'],
              [14, 'Anthropic'],
              [24, 'Gemini'],
              [43, 'DeepSeek'],
              [20, 'OpenRouter'],
              [60, 'New API'],
            ].map(([id, name]) => (
              <NativeSelectOption key={id} value={id}>
                {name}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Label>
        <Label>
          {t('Base URL')}
          <Input
            readOnly={props.restrictSensitive}
            type='url'
            {...form.register('base_url')}
          />
        </Label>
        <Label>
          {t('Models')}
          <Input
            placeholder='gpt-4o,gpt-4o-mini'
            required
            {...form.register('models')}
          />
        </Label>
        <Label>
          {t('API Key')}
          <PasswordInput
            autoComplete='new-password'
            readOnly={props.restrictSensitive}
            required={!r}
            {...form.register('key')}
          />
        </Label>
        <Button type='submit' disabled={save.isPending}>
          {t('Save')}
        </Button>
      </form>
    </Dialog>
  )
}

function targetDescription(target: SupplyTarget) {
  if (target.login_name) return `${target.login_name} @ ${target.url}`
  return target.mode ? target.url : target.name
}

export function TargetEditor(props: {
  targets: SupplyTarget[]
  close: () => void
  saved: () => void
}) {
  const { t } = useTranslation()
  const form = useForm({
    defaultValues: { id: '', url: '', username: '', password: '', token: '' },
  })
  const id = form.watch('id')
  const selected = props.targets.find((target) => target.id === id)
  const legacy = !!id && !selected?.mode
  const save = useMutation({
    gcTime: 0,
    retry: false,
    mutationFn: () => {
      const v = form.getValues()
      return supplyRequest(
        v.id ? `targets/${v.id}` : 'targets',
        v.id ? 'PUT' : 'POST',
        legacy
          ? { token: v.token }
          : {
              url: v.url,
              username: v.username,
              password: v.password,
              access_method: 'saved_account',
            }
      )
    },
    onSuccess: () => {
      form.reset()
      props.saved()
    },
  })
  return (
    <Dialog
      open
      title={t('Downstream instances')}
      onOpenChange={(v) => {
        if (!v) props.close()
      }}
    >
      <form
        className='grid gap-4'
        onSubmit={form.handleSubmit(() => save.mutate())}
      >
        <Label>
          {t('Target')}
          <NativeSelect {...form.register('id')}>
            <NativeSelectOption value=''>{t('Create')}</NativeSelectOption>
            {props.targets.map((row) => (
              <NativeSelectOption key={row.id} value={row.id}>
                {targetDescription(row)}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Label>
        {!id && (
          <Label>
            {t('Base URL')}
            <Input
              type='url'
              required
              autoComplete='url'
              {...form.register('url')}
            />
          </Label>
        )}
        {legacy ? (
          <Label>
            {t('Access token')}
            <PasswordInput
              required
              autoComplete='new-password'
              {...form.register('token')}
            />
          </Label>
        ) : (
          <>
            <Label>
              {t('Username')}
              <Input
                required
                autoComplete='off'
                {...form.register('username')}
              />
            </Label>
            <Label>
              {t('Password')}
              <PasswordInput
                required
                autoComplete='new-password'
                {...form.register('password')}
              />
            </Label>
            <p className='text-muted-foreground text-sm'>
              {t(
                'The reseller encrypts the customer password to submit channels and collect channel usage.'
              )}
            </p>
          </>
        )}
        <Button type='submit' disabled={save.isPending}>
          {t('Save')}
        </Button>
      </form>
    </Dialog>
  )
}

export function AssignmentEditor(props: {
  resource: SupplyResource
  targets: SupplyTarget[]
  close: () => void
  saved: () => void
}) {
  const { t } = useTranslation()
  const r = props.resource
  const form = useForm({
    defaultValues: {
      target: r.target,
      user_id: r.user_id || undefined,
      customer_group: r.customer_group || 'default',
    },
  })
  const selectedTarget = form.watch('target')
  const target = props.targets.find((x) => x.id === selectedTarget)
  const instance = !!target?.mode
  const account = target?.mode === 'customer_account'
  const save = useMutation({
    mutationFn: (v: {
      target: string
      user_id: number | undefined
      customer_group: string
    }) =>
      supplyRequest(`resources/${r.id}/assignment`, 'PUT', {
        ...v,
        user_id: instance ? 0 : v.user_id,
        customer_group: account ? v.customer_group : undefined,
        scope: instance ? 'instance' : 'user',
        revision: r.revision,
      }),
    onSuccess: props.saved,
  })
  return (
    <Dialog
      open
      title={t('Assign channel')}
      onOpenChange={(v) => {
        if (!v) props.close()
      }}
    >
      <form
        className='grid gap-4'
        onSubmit={form.handleSubmit((v) => save.mutate(v))}
      >
        <Label>
          {t('Target')}
          <NativeSelect required {...form.register('target')}>
            <NativeSelectOption value=''>—</NativeSelectOption>
            {props.targets.map((row) => (
              <NativeSelectOption key={row.id} value={row.id}>
                {targetDescription(row)}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </Label>
        {account && (
          <>
            <p>
              {target?.account_api
                ? t(
                    'The reseller writes this channel to the customer account and reads back its ID.'
                  )
                : t('Customer account lacks the required permissions')}
            </p>
            {!target?.usage_api && (
              <p role='status'>{t('Usage data is incomplete')}</p>
            )}
            <Label>
              {t('Group')}
              <Input required {...form.register('customer_group')} />
            </Label>
          </>
        )}
        {instance && !account && (
          <p className='text-muted-foreground text-sm'>
            {t(
              'Assign to this customer instance. Its administrator installs the upstream credentials.'
            )}
          </p>
        )}
        {!instance && (
          <Label>
            {t('User ID')}
            <Input
              required
              type='number'
              min='1'
              step='1'
              {...form.register('user_id', { valueAsNumber: true })}
            />
          </Label>
        )}
        <Button
          type='submit'
          disabled={save.isPending || (account && !target?.account_api)}
        >
          {t('Save')}
        </Button>
      </form>
    </Dialog>
  )
}
