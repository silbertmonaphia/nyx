import { useTranslation } from 'react-i18next';

import { setLocale, type Locale } from '~/i18n';

const OPTIONS: { locale: Locale; label: string }[] = [
  { locale: 'zh', label: '中' },
  { locale: 'en', label: 'EN' },
];

export function LocaleToggle() {
  const { t, i18n } = useTranslation();
  const active = i18n.language;

  return (
    <div
      role="group"
      aria-label={t('locale.switch')}
      title={t('locale.switch')}
      data-testid="locale-toggle"
      className="hidden md:inline-flex items-center gap-0.5 rounded-full border border-white/15 p-0.5"
    >
      {OPTIONS.map(({ locale, label }) => (
        <button
          key={locale}
          type="button"
          onClick={() => setLocale(locale)}
          aria-pressed={active === locale}
          data-testid={`locale-${locale}`}
          className={
            active === locale
              ? 'rounded-full bg-white/15 px-2 py-0.5 text-xs font-medium text-white'
              : 'rounded-full px-2 py-0.5 text-xs font-medium text-white/50 hover:text-white'
          }
        >
          {label}
        </button>
      ))}
    </div>
  );
}
