/**
 * Per-view document metadata.
 *
 * React 19 hoists `<title>`, `<meta>`, and `<link>` elements rendered
 * anywhere in the tree into `<head>` automatically — no provider or
 * third-party library required. This wrapper exists purely so callers
 * don't have to remember the meta-tag shape and to keep the two
 * attributes (title + description) in one place.
 *
 * Rendering this component multiple times in the same tree is fine;
 * React replaces the hoisted node in-place rather than accumulating
 * duplicates.
 */
export interface PageMetaProps {
  title: string;
  description?: string;
}

export function PageMeta({ title, description }: PageMetaProps) {
  return (
    <>
      <title>{title}</title>
      {description !== undefined && (
        <meta name="description" content={description} />
      )}
    </>
  );
}

export default PageMeta;