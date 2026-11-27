/** @type {import('next').NextConfig} */
const apiOrigin = (process.env.METERLY_API_ORIGIN || "http://127.0.0.1:8080").replace(/\/+$/, "");

const nextConfig = {
  allowedDevOrigins: ["*.e2b.app", "localhost", "127.0.0.1"],
  async rewrites() {
    return [
      {
        source: "/v1/:path*",
        destination: `${apiOrigin}/v1/:path*`,
      },
      {
        source: "/healthz",
        destination: `${apiOrigin}/healthz`,
      },
      {
        source: "/readyz",
        destination: `${apiOrigin}/readyz`,
      },
    ];
  },
};

export default nextConfig;
