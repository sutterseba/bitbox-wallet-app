/**
 * Copyright 2022 Shift Crypto AG
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { useTranslation } from 'react-i18next';
import { Entry } from '../../../components/guide/entry';
import { Guide } from '../../../components/guide/guide';
import { IAccount } from '../../../api/account';
import { isBitcoinOnly } from '../../account/utils';

type TAddAccountGuide = {
  accounts: IAccount[]
}

export const AddAccountGuide = ({ accounts }: TAddAccountGuide) => {
  const { t } = useTranslation();
  const hasOnlyBTCAccounts = accounts.every(({ coinCode }) => isBitcoinOnly(coinCode));
  return (
    <Guide>
      <Entry key="whatAreAccounts" entry={t('guide.accounts.whatAreAccounts')} />
      <Entry key="whyIsThisUseful" entry={t('guide.accounts.whyIsThisUseful')} />
      <Entry key="recoverAccounts" entry={t('guide.accounts.recoverAccounts')} />
      <Entry key="moveFunds" entry={t('guide.accounts.moveFunds')} />
      { !hasOnlyBTCAccounts && (
        <>
          <Entry key="supportedCoins" entry={{
            link: {
              text: t('guide.accounts.supportedCoins.link.text'),
              url: 'https://bitbox.swiss/coins/',
            },
            text: t('guide.accounts.supportedCoins.text'),
            title: t('guide.accounts.supportedCoins.title'),
          }} />
          <Entry key="howtoAddTokens" entry={t('guide.accounts.howtoAddTokens')} />
        </>
      )}
      <Entry key="howManyAccounts" entry={t('guide.accounts.howManyAccounts')} />
    </Guide>
  );
};
