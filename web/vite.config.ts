import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig, loadEnv, type ProxyOptions } from 'vite';

// Static SPA. The hub serves the built bundle itself, so every client talks to
// exactly one origin and there is no CORS surface to get wrong.
//
// In dev, proxy the API to the real hub, named in .env.local (git-ignored):
//   REMOTER_HUB=https://<hub>.<tailnet>.ts.net
// Its cross-origin guard refuses a page served from localhost, so the proxy
// presents the hub's own origin instead.
export default defineConfig(({ mode }) => {
	const hub = loadEnv(mode, '.', 'REMOTER_').REMOTER_HUB ?? 'https://hub.invalid';
	const toHub: ProxyOptions = {
		target: hub,
		changeOrigin: true,
		ws: true,
		headers: { origin: hub }
	};

	return {
		plugins: [
			sveltekit({
				compilerOptions: {
					runes: ({ filename }) =>
						filename.split(/[/\\]/).includes('node_modules') ? undefined : true
				},
				adapter: adapter({ fallback: 'index.html' })
			})
		],
		server: {
			proxy: { '/hub': toHub, '/d': toHub, '/dl': toHub }
		}
	};
});
