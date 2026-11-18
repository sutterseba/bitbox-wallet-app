// SPDX-License-Identifier: Apache-2.0

import { apiGet, apiPost } from '@/utils/request';
import { subscribeEndpoint, TSubscriptionCallback } from './subscribe';

/**
 * Describes the cached result of the backend update check.
 */
export type TUpdateFile = {
  current: string;
  version: string;
  description: string;
  descriptionTranslations?: { [language: string]: string };
};

export type TUpdateState = {
  revision: number;
  update: TUpdateFile | null;
  installation?: {
    status: 'available' | 'downloading' | 'ready' | 'installing' | 'error';
    errorCode?: 'download' | 'install';
  };
};

type TUpdateResult = { success: true } | { success: false; errorCode?: string };

export const getVersion = (): Promise<string> => {
  return apiGet('version');
};

export const getUpdate = (): Promise<TUpdateState> => {
  return apiGet('update');
};

export const checkUpdate = (): Promise<TUpdateState> => {
  return apiGet('update?about=1');
};

export const downloadUpdate = (): Promise<TUpdateResult> => {
  return apiPost('update/download');
};

export const installUpdate = (messages: { installFailed: string; relaunchFailed: string }): Promise<TUpdateResult> => {
  return apiPost('update/install', messages);
};

export const subscribeUpdate = (
  cb: TSubscriptionCallback<TUpdateState>
) => (
  subscribeEndpoint('update', cb)
);
