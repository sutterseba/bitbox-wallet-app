// SPDX-License-Identifier: Apache-2.0

import { useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { runningInAndroid } from '@/utils/env';
import { downloadUpdate, getUpdate, installUpdate, subscribeUpdate } from '@/api/version';
import { Button } from '@/components/forms';
import { Status } from '@/components/status/status';
import { AppDownloadLink } from '@/components/appdownloadlink/appdownloadlink';
import { SpinnerRingAnimated } from '@/components/spinner/SpinnerAnimation';
import { useSync } from '@/hooks/api';
import { useMountedRef } from '@/hooks/mount';
import style from './update.module.css';

type TOperation = 'download' | 'install';

export const Update = () => {
  const { t, i18n } = useTranslation();
  const state = useSync(getUpdate, subscribeUpdate, state => state.revision);
  const mounted = useMountedRef();
  const requestInProgress = useRef(false);
  const [pending, setPending] = useState<TOperation | null>(null);
  const [requestError, setRequestError] = useState<{ revision: number; operation: TOperation } | null>(null);
  const file = state?.update;
  if (!file) {
    return null;
  }
  const description = file.descriptionTranslations?.[i18n.resolvedLanguage ?? 'en'] || file.description;
  const announcement = (
    <>
      {t('app.upgrade', { current: file.current, version: file.version })}
      {' '}
      {description}
    </>
  );

  if (state.installation) {
    const { installation } = state;
    const error = (
      requestError?.revision === state.revision
        ? requestError.operation
        : installation.status === 'error' ? installation.errorCode || 'download' : null
    );
    const installing = installation.status === 'installing' || pending === 'install';
    const downloading = installation.status === 'downloading' || (pending === 'download' && installation.status !== 'ready');
    const busy = installing || downloading;
    const ready = installation.status === 'ready';

    const start = async (operation: TOperation) => {
      if (requestInProgress.current) {
        return;
      }
      requestInProgress.current = true;
      setPending(operation);
      setRequestError(null);
      try {
        const result = await (operation === 'download' ? downloadUpdate() : installUpdate({
          installFailed: t('app.update.installFailedAfterClose'),
          relaunchFailed: t('app.update.relaunchFailed'),
        }));
        if (!result.success && mounted.current) {
          setRequestError({ revision: state.revision, operation });
        }
      } catch {
        if (mounted.current) {
          setRequestError({ revision: state.revision, operation });
        }
      } finally {
        requestInProgress.current = false;
        if (mounted.current) {
          setPending(null);
        }
      }
    };

    return (
      <Status
        dismissibleKey={installation.status === 'available' && !busy && !error ? `update-${file.version}` : ''}
        type="info">
        <span role="status" aria-live="polite">
          {busy ? (
            <>
              {t(installing ? 'app.update.installing' : 'app.update.downloading')}
              <span className={style.spinner} aria-hidden="true"><SpinnerRingAnimated /></span>
            </>
          ) : (
            <>
              {error
                ? t(error === 'install' ? 'app.update.installError' : 'app.update.downloadError')
                : ready
                  ? t('app.update.ready')
                  : announcement}
              {' '}
              <Button
                transparent
                inline
                className={style.action}
                disabled={pending !== null}
                onClick={() => start(ready && !error ? 'install' : 'download')}>
                {t(error ? 'generic.retry' : ready ? 'app.update.installNow' : 'app.update.install')}
              </Button>
            </>
          )}
        </span>
      </Status>
    );
  }

  return (
    <Status dismissibleKey={`update-${file.version}`} type="info">
      {announcement}
      {' '}
      {/* Don't show download link on Android because they should update from stores */}
      {!runningInAndroid() && <AppDownloadLink className={style.link} />}
    </Status>
  );
};
