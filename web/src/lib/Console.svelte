<script lang="ts">
	// Tier 2. A real terminal over one WebSocket: JSON control frames up, raw
	// bytes down. The agent owns the PTY, so no SSH credentials are involved.
	import { onMount } from 'svelte';
	import { Terminal } from '@xterm/xterm';
	import { FitAddon } from '@xterm/addon-fit';
	import '@xterm/xterm/css/xterm.css';
	import { agent } from '$lib/agent.svelte';

	let { onbytes }: { onbytes?: (n: number) => void } = $props();

	let viewport: HTMLDivElement;
	let surface: HTMLDivElement;
	let status = $state<'connecting' | 'open' | 'closed'>('connecting');
	let detail = $state('');

	// Fitting columns to a phone's width gives ~30 columns, which wraps every
	// real command into noise. Instead the terminal keeps a proper width and the
	// viewport scrolls sideways - the shell's idea of the window stays honest.
	const WIDTHS = [80, 100, 120, 160] as const;
	let cols = $state<number>(80);

	let term: Terminal | null = null;
	let fit: FitAddon | null = null;
	let send: (msg: object) => void = () => {};

	/** Rows come from the available height; columns are chosen, not fitted. */
	function applySize() {
		if (!term || !fit) return;
		const proposed = fit.proposeDimensions();
		const rows = proposed?.rows && proposed.rows > 0 ? proposed.rows : term.rows;
		term.resize(cols, rows);
		send({ type: 'resize', cols, rows });
	}

	// Re-apply when the width selector changes.
	$effect(() => {
		cols;
		applySize();
	});

	onMount(() => {
		const t = new Terminal({
			fontSize: 13,
			fontFamily: 'Consolas, "Cascadia Mono", ui-monospace, monospace',
			cursorBlink: true,
			theme: {
				background: '#0b0d10',
				foreground: '#e6eaf0',
				cursor: '#5eb0ef',
				selectionBackground: '#2a323c'
			},
			scrollback: 5000,
			convertEol: false
		});
		term = t;

		const f = new FitAddon();
		fit = f;
		t.loadAddon(f);
		t.open(surface);
		t.resize(cols, t.rows);

		const params = new URLSearchParams({ cols: String(cols), rows: String(t.rows) });
		const socket = new WebSocket(`${agent.socketUrl('/api/console')}?${params}`);
		socket.binaryType = 'arraybuffer';

		send = (msg: object) => {
			if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(msg));
		};

		socket.onopen = () => {
			status = 'open';
			applySize();
			t.focus();
		};

		socket.onmessage = (ev) => {
			const bytes = new Uint8Array(ev.data as ArrayBuffer);
			onbytes?.(bytes.byteLength);
			t.write(bytes);
		};

		socket.onclose = (ev) => {
			status = 'closed';
			detail = ev.reason || `closed (${ev.code})`;
			t.write('\r\n\x1b[90m-- session ended --\x1b[0m\r\n');
		};

		socket.onerror = () => {
			detail = 'connection failed';
		};

		t.onData((data) => send({ type: 'data', data }));

		// Only height changes should re-derive rows; width is ours to choose.
		const observer = new ResizeObserver(() => applySize());
		observer.observe(viewport);
		window.addEventListener('orientationchange', applySize);

		return () => {
			observer.disconnect();
			window.removeEventListener('orientationchange', applySize);
			socket.close();
			t.dispose();
			term = null;
			fit = null;
		};
	});

	/** Sticky modifiers are non-negotiable on a touchscreen: you cannot hold Ctrl. */
	function key(seq: string) {
		send({ type: 'data', data: seq });
		term?.focus();
	}
</script>

<div class="wrap">
	<div class="bar">
		<span class="dot" class:live={status === 'open'}></span>
		<span class="muted">{status}{detail ? ` · ${detail}` : ''}</span>
		<span class="spacer"></span>
		<label class="width">
			width
			<select bind:value={cols}>
				{#each WIDTHS as w (w)}
					<option value={w}>{w}</option>
				{/each}
			</select>
		</label>
	</div>

	<!-- The viewport scrolls; the surface keeps its true width. -->
	<div class="viewport" bind:this={viewport}>
		<div class="surface" bind:this={surface}></div>
	</div>

	<div class="keys">
		<button onclick={() => key('\x03')} title="Ctrl+C">^C</button>
		<button onclick={() => key('\t')}>Tab</button>
		<button onclick={() => key('\x1b')}>Esc</button>
		<button onclick={() => key('\x1b[A')}>↑</button>
		<button onclick={() => key('\x1b[B')}>↓</button>
		<button onclick={() => key('\x1b[D')}>←</button>
		<button onclick={() => key('\x1b[C')}>→</button>
	</div>
</div>

<style>
	.wrap {
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
	}

	.bar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.78rem;
	}
	.spacer { flex: 1; }

	.dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--err);
	}
	.dot.live { background: var(--ok); }

	.width { display: flex; align-items: center; gap: 0.35rem; color: var(--muted); }
	.width select {
		background: var(--surface);
		color: var(--text);
		border: 1px solid var(--line);
		border-radius: 6px;
		min-height: 32px;
		padding: 0 0.35rem;
	}

	.viewport {
		height: 58vh;
		padding: 0.5rem;
		background: #0b0d10;
		border: 1px solid var(--line);
		border-radius: 8px;
		/* Sideways scrolling instead of squeezing the shell into phone width. */
		overflow-x: auto;
		overflow-y: hidden;
		-webkit-overflow-scrolling: touch;
	}

	.surface {
		display: inline-block;
		height: 100%;
	}

	.keys {
		display: flex;
		gap: 0.4rem;
		overflow-x: auto;
		padding-bottom: 0.2rem;
	}
	.keys button {
		flex: none;
		min-width: 48px;
		min-height: 40px;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 8px;
		font-size: 0.85rem;
	}
	.keys button:active { background: var(--surface-2); }

	.muted { color: var(--muted); }
</style>
