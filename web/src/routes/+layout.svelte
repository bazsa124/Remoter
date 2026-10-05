<script lang="ts">
	let { children } = $props();
</script>

<svelte:head>
	<meta name="theme-color" content="#0b0d10" />
</svelte:head>

{@render children()}

<style>
	:global(:root) {
		--bg: #0b0d10;
		--surface: #14181d;
		--surface-2: #1c222a;
		--line: #2a323c;
		--text: #e6eaf0;
		--muted: #8b98a8;
		--accent: #5eb0ef;
		--ok: #4ade80;
		--warn: #fbbf24;
		--err: #f87171;

		color-scheme: dark;
	}

	:global(*) {
		box-sizing: border-box;
	}

	:global(body) {
		margin: 0;
		background: var(--bg);
		color: var(--text);
		font: 15px/1.5 system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
		/* Room for the phone's home indicator. */
		padding-bottom: env(safe-area-inset-bottom);
	}

	:global(button) {
		font: inherit;
		color: inherit;
		cursor: pointer;
	}
</style>
