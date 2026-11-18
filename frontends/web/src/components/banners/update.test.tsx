// SPDX-License-Identifier: Apache-2.0

import '../../../__mocks__/i18n';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { TUpdateFile, TUpdateState } from '@/api/version';
import { Update } from './update';

const versionApiMocks = vi.hoisted(() => ({
  getUpdate: vi.fn(),
  subscribeUpdate: vi.fn(),
  downloadUpdate: vi.fn(),
  installUpdate: vi.fn(),
}));

vi.mock('@/api/version', () => versionApiMocks);

vi.mock('@/contexts/ConfigProvider', () => ({
  useConfig: () => ({
    config: { frontend: {} },
    setConfig: vi.fn(),
  }),
}));

vi.mock('@/hooks/darkmode', () => ({
  useDarkmode: () => ({ isDarkMode: false }),
}));

const update: TUpdateFile = {
  current: '4.51.3',
  version: '4.52.0',
  description: 'A new version is ready.',
};
const noUpdateState: TUpdateState = { revision: 0, update: null };
const updateState: TUpdateState = { revision: 1, update };
const installationState = (
  status: NonNullable<TUpdateState['installation']>['status'],
  revision: number,
  errorCode?: 'download' | 'install',
): TUpdateState => ({
  revision,
  update,
  installation: { status, errorCode },
});

describe('components/banners/update', () => {
  let notifyUpdate: ((state: TUpdateState) => void) | undefined;

  beforeEach(() => {
    vi.clearAllMocks();
    notifyUpdate = undefined;
    versionApiMocks.getUpdate.mockResolvedValue(noUpdateState);
    versionApiMocks.downloadUpdate.mockResolvedValue({ success: true });
    versionApiMocks.installUpdate.mockResolvedValue({ success: true });
    versionApiMocks.subscribeUpdate.mockImplementation((cb: typeof notifyUpdate) => {
      notifyUpdate = cb;
      return () => {};
    });
  });

  it('renders a cached update', async () => {
    versionApiMocks.getUpdate.mockResolvedValue(updateState);

    render(<Update />);

    expect(await screen.findByText(update.description, { exact: false })).toBeInTheDocument();
    expect(versionApiMocks.subscribeUpdate).toHaveBeenCalledOnce();
  });

  it('renders an update received from the subscription', async () => {
    const { container } = render(<Update />);

    await waitFor(() => expect(versionApiMocks.subscribeUpdate).toHaveBeenCalledOnce());
    expect(container.firstChild).toBeNull();

    act(() => notifyUpdate?.(updateState));

    expect(screen.getByText(update.description, { exact: false })).toBeInTheDocument();
  });

  // The banner shows the feed version and description; its action downloads once, then installs on a separate click.
  it('follows download, ready and install notifications with separate user actions', async () => {
    const user = userEvent.setup();
    versionApiMocks.getUpdate.mockResolvedValue(installationState('available', 1));
    render(<Update />);

    const button = await screen.findByRole('button', { name: 'Install' });
    expect(screen.getByRole('status')).toHaveTextContent(
      `A new version of this app is available! Please upgrade from ${update.current} to ${update.version}. ${update.description}`
    );
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    await act(async () => {
      fireEvent.click(button);
      fireEvent.click(button);
    });
    expect(versionApiMocks.downloadUpdate).toHaveBeenCalledOnce();
    expect(versionApiMocks.installUpdate).not.toHaveBeenCalled();

    act(() => notifyUpdate?.(installationState('downloading', 2)));
    expect(screen.getByRole('status')).toHaveTextContent('Downloading update');
    expect(screen.queryByRole('button')).not.toBeInTheDocument();

    act(() => notifyUpdate?.(installationState('ready', 3)));
    expect(screen.getByRole('status')).toHaveTextContent('Update ready to install.');
    expect(screen.getAllByRole('button')).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Install now' }));
    expect(versionApiMocks.installUpdate).toHaveBeenCalledOnce();
    expect(versionApiMocks.installUpdate).toHaveBeenCalledWith({
      installFailed: 'The update could not be installed. BitBoxApp will try to reopen.',
      relaunchFailed: 'BitBoxApp could not reopen. Please open it manually.',
    });

    act(() => notifyUpdate?.(installationState('installing', 4)));
    expect(screen.getByRole('status')).toHaveTextContent('Installing update. The app will restart.');
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  // A failed request must leave an actionable retry instead of a permanent spinner.
  it('allows retry after a network failure', async () => {
    const user = userEvent.setup();
    versionApiMocks.downloadUpdate.mockRejectedValueOnce(new Error('offline'));
    versionApiMocks.getUpdate.mockResolvedValue(installationState('available', 1));
    render(<Update />);

    await user.click(await screen.findByRole('button', { name: 'Install' }));
    expect(await screen.findByText('The update could not be downloaded or verified. Please try again.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    expect(versionApiMocks.downloadUpdate).toHaveBeenCalledTimes(2);
  });

  // Either backend failure retries the download so a rejected installer is not reused.
  it.each(['download', 'install'] as const)('retries a backend %s failure', async operation => {
    const user = userEvent.setup();
    versionApiMocks.getUpdate.mockResolvedValue(installationState('error', 1, operation));
    render(<Update />);

    await user.click(await screen.findByRole('button', { name: 'Retry' }));

    expect(versionApiMocks.downloadUpdate).toHaveBeenCalledOnce();
    expect(versionApiMocks.installUpdate).not.toHaveBeenCalled();
  });

  // A late network rejection cannot replace a newer authoritative ready notification.
  it('ignores an old request failure after a newer state arrives', async () => {
    let rejectDownload: (reason: Error) => void = () => {};
    versionApiMocks.downloadUpdate.mockReturnValue(new Promise((_, reject) => {
      rejectDownload = reject;
    }));
    versionApiMocks.getUpdate.mockResolvedValue(installationState('available', 1));
    render(<Update />);
    fireEvent.click(await screen.findByRole('button', { name: 'Install' }));

    act(() => notifyUpdate?.(installationState('ready', 2)));
    await act(async () => rejectDownload(new Error('response lost')));

    expect(screen.getByRole('button', { name: 'Install now' })).toBeEnabled();
    expect(screen.queryByText('The update could not be downloaded or verified. Please try again.')).not.toBeInTheDocument();
  });
});
