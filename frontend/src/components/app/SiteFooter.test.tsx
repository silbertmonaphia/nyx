import { render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SiteFooter } from './SiteFooter';

const FILING = '粤ICP备2026191358号';
const ICP_URL = 'https://beian.miit.gov.cn/';

describe('SiteFooter', () => {
  afterEach(() => {
    vi.unstubAllEnvs();
  });

  it('renders nothing when the filing number is empty', () => {
    vi.stubEnv('VITE_ICP_BEIAN', '');
    const { container } = render(<SiteFooter />);

    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing when the filing number is only whitespace', () => {
    vi.stubEnv('VITE_ICP_BEIAN', '   ');
    const { container } = render(<SiteFooter />);

    expect(container).toBeEmptyDOMElement();
  });

  it('links the configured filing number to the MIIT filing site', () => {
    vi.stubEnv('VITE_ICP_BEIAN', FILING);
    render(<SiteFooter />);

    const link = screen.getByTestId('icp-beian-link');
    expect(link).toHaveTextContent(FILING);
    expect(link).toHaveAttribute('href', ICP_URL);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link.getAttribute('rel')).toContain('noopener');
    expect(link.getAttribute('rel')).toContain('noreferrer');
  });

  it('trims surrounding whitespace from the filing number', () => {
    vi.stubEnv('VITE_ICP_BEIAN', `  ${FILING}  `);
    render(<SiteFooter />);

    expect(screen.getByTestId('icp-beian-link')).toHaveTextContent(FILING);
  });

  it('raises only the link above an open modal overlay', () => {
    vi.stubEnv('VITE_ICP_BEIAN', FILING);
    const { unmount } = render(<SiteFooter />);
    expect(screen.getByTestId('icp-beian-link').className).not.toContain('z-[60]');
    unmount();

    render(<SiteFooter raised />);
    expect(screen.getByTestId('icp-beian-link').className).toContain('z-[60]');
  });
});
