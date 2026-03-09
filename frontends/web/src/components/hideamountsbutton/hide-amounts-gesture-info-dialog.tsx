// SPDX-License-Identifier: Apache-2.0

import { useTranslation } from 'react-i18next';
import { Dialog, DialogButtons } from '@/components/dialog/dialog';
import { Button } from '@/components/forms/button';
import { FlipGestureLight } from '@/components/icon';
import styles from './hide-amounts-gesture-info-dialog.module.css';

type Props = {
  open: boolean;
  onClose: () => void;
};

export const HideAmountsGestureInfoDialog = ({ open, onClose }: Props) => {
  const { t } = useTranslation();

  return (
    <Dialog title={t('newSettings.appearance.hideAmounts.gestureInfo.title')} medium open={open}>
      <FlipGestureLight className={styles.icon} />
      <p>{t('newSettings.appearance.hideAmounts.gestureInfo.text')}</p>
      <DialogButtons>
        <Button primary onClick={onClose}>
          {t('button.ok')}
        </Button>
      </DialogButtons>
    </Dialog>
  );
};
