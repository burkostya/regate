import { env } from '$env/dynamic/private';
import type { RequestHandler } from './$types';

// Same-origin management API in BOTH development and built Node deployments.
// Never expose the backend origin or admin bearer in browser-visible config.
const proxy: RequestHandler = async ({ request, url }) => {
  if (!url.pathname.startsWith('/api/') || url.pathname.includes('\\')) {
    return Response.json({ error: 'Invalid API path.' }, { status: 400 });
  }
  let target: URL;
  try {
    target = new URL(env.REGATE_PROXY?.trim() || 'http://127.0.0.1:8080');
    if (!['http:', 'https:'].includes(target.protocol) || target.username ||
        target.password || target.pathname !== '/' || target.search || target.hash) {
      throw new Error('Invalid backend origin');
    }
    target.pathname = url.pathname;
    target.search = url.search;
  } catch {
    return Response.json({ error: 'Gateway proxy is misconfigured.' }, { status: 500 });
  }

  // Forward only credentials and media-negotiation headers needed by
  // management REST. Cookies, Host, Forwarded, etc. must not cross this hop.
  const headers = new Headers();
  for (const key of ['authorization', 'content-type', 'accept']) {
    const value = request.headers.get(key);
    if (value !== null) headers.set(key, value);
  }
  const init: RequestInit & { duplex?: 'half' } = {
    method: request.method,
    headers,
    redirect: 'manual', // do not forward the bearer on a remote redirect
    cache: 'no-store'
  };
  if (request.method !== 'GET' && request.body) {
    // Stream large uploads rather than buffering privileged request data.
    init.body = request.body;
    init.duplex = 'half';
  }

  let result: Response;
  try {
    result = await fetch(target, init);
  } catch {
    return Response.json({ error: 'Gateway unavailable.' }, { status: 502 });
  }
  if (result.status >= 300 && result.status < 400) {
    await result.body?.cancel();
    return Response.json({ error: 'Backend redirect refused.' }, { status: 502 });
  }
  const responseHeaders = new Headers({
    'Cache-Control': 'no-store',
    'X-Content-Type-Options': 'nosniff'
  });
  const contentType = result.headers.get('content-type');
  if (contentType) responseHeaders.set('Content-Type', contentType);
  return new Response(result.body, { status: result.status, headers: responseHeaders });
};

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
