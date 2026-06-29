export default function PageNotFoundView() {
  return (
    <div className="flex flex-col items-center justify-center h-full min-h-[400px] text-[hsl(var(--muted-foreground))]">
      <h1 className="text-6xl font-bold mb-4">404</h1>
      <p className="text-xl">Page not found</p>
    </div>
  );
}
