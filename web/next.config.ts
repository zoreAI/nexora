import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { NextConfig } from 'next'

const projectRoot = join(dirname(fileURLToPath(import.meta.url)), '..')

function readAppVersion() {
    try {
        const version = readFileSync(join(projectRoot, 'VERSION'), 'utf-8').trim()
        return version || '0.0.0'
    } catch {
        return '0.0.0'
    }
}

const appVersion = process.env.NEXT_PUBLIC_APP_VERSION || readAppVersion()
const isDevelopment = process.env.NODE_ENV === 'development'

const nextConfig: NextConfig = {
    allowedDevOrigins: ['127.0.0.1'],
    env: {
        NEXT_PUBLIC_APP_VERSION: appVersion,
    },
    output: isDevelopment ? undefined : 'export',
    trailingSlash: true,
    ...(isDevelopment ? { skipTrailingSlashRedirect: true } : {}),
    images: {
        unoptimized: true,
    },
    typescript: {
        ignoreBuildErrors: true,
    },
    ...(isDevelopment ? { experimental: { proxyTimeout: 300_000 } } : {}),
    ...(isDevelopment ? {
        async rewrites() {
            const backend = process.env.NEXT_PUBLIC_DEV_BACKEND || 'http://127.0.0.1:3000'
            return [
                { source: '/auth/:path*', destination: `${backend}/auth/:path*` },
                { source: '/api/:path*', destination: `${backend}/api/:path*` },
                { source: '/v1/:path*', destination: `${backend}/v1/:path*` },
                { source: '/images/:path*', destination: `${backend}/images/:path*` },
            ]
        },
    } : {}),
}

export default nextConfig
