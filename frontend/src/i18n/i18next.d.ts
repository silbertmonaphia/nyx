import 'i18next';

import type { zh } from './zh';

declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'translation';
    resources: typeof zh;
  }
}
