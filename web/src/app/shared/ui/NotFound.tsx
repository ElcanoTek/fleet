// The not-found page: an unknown URL, an unknown or expired share link, an
// unknown guide. The server answers these with a 404 status and this page.
// Same words and layout as the framework page it replaces.
export function NotFound() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center text-[var(--color-text-primary)]">
      <div className="flex items-center">
        <h1 className="mr-5 border-r border-[var(--color-border-strong)] pr-6 text-2xl font-medium leading-[49px]">404</h1>
        <h2 className="text-sm font-normal leading-[49px]">This page could not be found.</h2>
      </div>
    </main>
  );
}
