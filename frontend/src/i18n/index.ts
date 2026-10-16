import i18next from 'i18next';
import { initReactI18next } from 'react-i18next';

import { en } from './en';
import { zh } from './zh';

export const SUPPORTED_LOCALES = ['zh', 'en'] as const;
export type Locale = (typeof SUPPORTED_LOCALES)[number];

export const DEFAULT_LOCALE: Locale = 'zh';
export const LOCALE_STORAGE_KEY = 'nyx.locale';

export function isLocale(value: unknown): value is Locale {
  return value === 'zh' || value === 'en';
}

function readStoredLocale(): Locale | null {
  try {
    const raw = window.localStorage.getItem(LOCALE_STORAGE_KEY);
    return isLocale(raw) ? raw : null;
  } catch {
    return null;
  }
}

function writeStoredLocale(locale: Locale): void {
  try {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    return;
  }
}

function applyDocumentLang(locale: Locale): void {
  if (typeof document !== 'undefined') {
    document.documentElement.lang = locale;
  }
}

export function resolveLocale(): Locale {
  return readStoredLocale() ?? DEFAULT_LOCALE;
}

void i18next.use(initReactI18next).init({
  resources: { zh, en },
  lng: resolveLocale(),
  fallbackLng: DEFAULT_LOCALE,
  supportedLngs: [...SUPPORTED_LOCALES],
  interpolation: { escapeValue: false },
});

applyDocumentLang(resolveLocale());

i18next.on('languageChanged', (language) => {
  const locale = isLocale(language) ? language : DEFAULT_LOCALE;
  applyDocumentLang(locale);
  writeStoredLocale(locale);
});

export function setLocale(locale: Locale): void {
  void i18next.changeLanguage(locale);
}

export const i18n = i18next;
export const t = i18next.t;

export default i18next;
