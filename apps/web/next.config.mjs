/** @type {import('next').NextConfig} */
const nextConfig = {
  // Required by apps/web/Dockerfile. `next build` only emits
  // .next/standalone/server.js when this is set; without it the runtime stage
  // has nothing to run and the image build fails at the COPY step.
  //
  // Standalone also means the runtime image carries only the traced subset of
  // node_modules instead of the full ~400MB install.
  output: 'standalone',
};

export default nextConfig;
