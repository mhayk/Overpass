import type { Metadata } from 'next';

import './globals.css';

export const metadata: Metadata = {
  title: 'Overpass — satellite tasking',
  description: 'Tasking, feasibility and collection planning for a SAR constellation',
};

/**
 * A server component, and it stays one.
 *
 * Server components are the default here rather than the exception. Only the
 * globe and the submission form genuinely need the browser, and marking a
 * layout as a client component would drag every child into the bundle with it.
 */
export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>): React.JSX.Element {
  return (
    <html lang="en" className="h-full">
      <body className="h-full bg-slate-950 text-slate-200 antialiased">{children}</body>
    </html>
  );
}
