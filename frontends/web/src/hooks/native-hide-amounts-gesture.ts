// SPDX-License-Identifier: Apache-2.0

import { useEffect } from 'react';

export const useNativeHideAmountsGesture = (toggleHideAmounts: () => void) => {
  useEffect(() => {
    window.onNativeToggleHideAmountsGesture = () => {
      toggleHideAmounts();
      return true;
    };

    return () => {
      delete window.onNativeToggleHideAmountsGesture;
    };
  }, [toggleHideAmounts]);
};
