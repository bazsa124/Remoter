<script lang="ts">
	// Tier 3 "Live". Low frame rate, tile-differenced desktop you can click - the
	// top of the ladder. On a Windows device it follows the input desktop, so the
	// lock screen and UAC prompts are visible and clickable too.
	//
	// Frame protocol, binary because at a few frames a second a JSON envelope per
	// frame would be a real share of the bandwidth this tier exists to save:
	//
	//   header 13 bytes: w, h, cols, rows, tileSize, tileCount (uint16 BE), keyframe (u8)
	//   then per tile:   index (uint16), length (uint32), JPEG bytes
	import { onMount } from 'svelte';
	import { agent } from '$lib/agent.svelte';

	let {
		onbytes,
		onlocked,
		os = ''
	}: { onbytes?: (n: number) => void; onlocked?: () => void; os?: string } = $props();

	// A phone target has no mouse: a one-finger drag is a swipe, and it has
	// Back/Home/Recents where a PC has Esc and Tab.
	const phone = $derived(os === 'android');

	let canvas: HTMLCanvasElement;
	let shell: HTMLDivElement;
	let status = $state<'connecting' | 'live' | 'reconnecting' | 'closed'>('connecting');
	let detail = $state('');
	let immersive = $state(false);

	let fps = $state(3);
	let quality = $state(35);
	let scale = $state(0.4);

	let kbps = $state(0);
	let frameBytes: { at: number; n: number }[] = [];

	let socket: WebSocket | null = null;
	let ctx: CanvasRenderingContext2D | null = null;

	// Decoding is async, so frames can arrive faster than they are drawn. Holding
	// every in-flight frame is an unbounded queue - memory climbs for as long as
	// the tier is open. Keep at most ONE frame waiting and discard anything older:
	// a stale desktop frame has no value once a newer one exists.
	let decoding = false;
	let pending: ArrayBuffer | null = null;
	let dropped = $state(0);
	// Dropping a frame loses the tiles it carried. The server only sends tiles it
	// believes changed, so those regions would stay stale until the next
	// keyframe - ask for a fresh one instead. Throttled, because a keyframe is
	// expensive and a struggling link must not be asked for them continuously.
	let lastResync = 0;
	let attempt = 0;
	let stopped = false;
	let retryTimer: ReturnType<typeof setTimeout> | null = null;

	// --- viewport: client-side only, the host is never resized -----------------
	let zoom = $state(1);
	let panX = $state(0);
	let panY = $state(0);

	// 1:1 renders the host's pixels at their true size instead of fitting them to
	// the viewport. On a phone that is useless - the desktop is far wider than the
	// screen - but from another laptop it is the difference between a scaled,
	// slightly soft picture and one that looks native. It sends full-resolution
	// frames, so it is opt-in and labelled with what it costs.
	let oneToOne = $state(false);

	const MAX_ZOOM = 4;
	const WHEEL_NOTCH = 120; // one detent, as Windows counts them
	const SCROLL_PX_PER_NOTCH = 44;

	/**
	 * Sends a control or input message.
	 *
	 * Returns false when the socket is not open. Silently dropping input is how
	 * this tier appears to "die after the first click": one dropped connection
	 * and every later tap goes nowhere with nothing on screen to say so.
	 */
	function send(msg: object): boolean {
		if (socket?.readyState !== WebSocket.OPEN) {
			if (status === 'live') {
				status = 'reconnecting';
				detail = 'input not delivered';
			}
			return false;
		}
		socket.send(JSON.stringify(msg));
		return true;
	}

	$effect(() => {
		const wire = oneToOne ? 1 : scale;
		send({ type: 'config', config: { fps, quality, scale: wire, monitor: 0 } });
	});

	function toggleOneToOne() {
		oneToOne = !oneToOne;
		// Zoom on top of 1:1 is two scaling systems fighting; reset it.
		zoom = 1;
		panX = 0;
		panY = 0;
	}

	async function draw(buf: ArrayBuffer) {
		const view = new DataView(buf);
		const w = view.getUint16(0);
		const h = view.getUint16(2);
		const cols = view.getUint16(4);
		const tile = view.getUint16(8);
		const count = view.getUint16(10);

		if (canvas.width !== w || canvas.height !== h) {
			canvas.width = w;
			canvas.height = h;
			ctx = canvas.getContext('2d');
		}
		if (!ctx) ctx = canvas.getContext('2d');

		let offset = 13;
		for (let i = 0; i < count; i++) {
			const index = view.getUint16(offset);
			const length = view.getUint32(offset + 2);
			offset += 6;

			const bytes = new Uint8Array(buf, offset, length);
			offset += length;

			try {
				const bitmap = await createImageBitmap(new Blob([bytes], { type: 'image/jpeg' }));
				ctx?.drawImage(bitmap, (index % cols) * tile, Math.floor(index / cols) * tile);
				bitmap.close();
			} catch {
				// A single undecodable tile must not stop the rest of the frame.
			}
		}
	}

	/** Draws one frame, then the newest frame that queued behind it, if any. */
	async function pump(first: ArrayBuffer) {
		decoding = true;
		try {
			let next: ArrayBuffer | null = first;
			while (next) {
				const buf = next;
				next = null;
				await draw(buf);
				next = pending;
				pending = null;
			}
		} finally {
			decoding = false;
		}
	}

	function requestResync() {
		const now = performance.now();
		if (now - lastResync < 1000) return;
		lastResync = now;
		send({ type: 'resync' });
	}

	// Smooth mode (phones): a screen recording instead of screenshots, after
	// someone at the phone taps "Start now". The phone reports each step.
	let smooth = $state<'off' | 'requested' | 'on' | 'declined' | 'stopped' | 'unsupported'>('off');

	function status_(text: string) {
		try {
			const msg = JSON.parse(text);
			if (msg.type !== 'status' || !msg.smooth) return;
			smooth = msg.smooth;
			if (smooth === 'declined') detail = 'smooth was declined on the phone';
			else if (smooth === 'stopped') detail = 'smooth was stopped on the phone';
			else if (smooth === 'on' || smooth === 'requested') detail = '';
		} catch {
			/* not ours */
		}
	}

	function toggleSmooth() {
		send({ type: 'smooth', on: smooth !== 'on' && smooth !== 'requested' });
	}

	function track(n: number) {
		onbytes?.(n);
		const now = performance.now();
		frameBytes.push({ at: now, n });
		frameBytes = frameBytes.filter((f) => now - f.at < 3000);
		kbps = Math.round((frameBytes.reduce((s, f) => s + f.n, 0) * 8) / 3 / 1000);
	}

	function connect() {
		if (stopped) return;

		const ws = new WebSocket(agent.socketUrl('/api/live'));
		ws.binaryType = 'arraybuffer';
		socket = ws;

		ws.onopen = () => {
			status = 'live';
			detail = '';
			attempt = 0;
			send({ type: 'config', config: { fps, quality, scale, monitor: 0 } });
			// A new connection starts with a fresh tile cache on the server, so the
			// first frame is a keyframe anyway - but say so explicitly rather than
			// relying on that.
			lastResync = 0;
			requestResync();
		};

		ws.onmessage = (ev) => {
			// Text messages are the node's status; frames are binary.
			if (typeof ev.data === 'string') {
				status_(ev.data);
				return;
			}
			const buf = ev.data as ArrayBuffer;
			track(buf.byteLength);

			if (decoding) {
				if (pending) {
					dropped++;
					requestResync();
				}
				pending = buf; // newest wins
				return;
			}
			void pump(buf);
		};

		ws.onclose = (ev) => {
			socket = null;
			if (stopped) return;
			status = 'reconnecting';
			detail = ev.reason || `dropped (${ev.code})`;
			// The hub refuses the handshake once the PIN grant lapses, and a
			// refused handshake carries no reason. Ask before retrying, or this
			// would reconnect forever against a closed door.
			const retry = () => {
				// Same discipline as the Tier 1 socket: back off with jitter
				// rather than hammering a phone's radio.
				const delay = Math.min(15000, 400 * 2 ** attempt++);
				retryTimer = setTimeout(connect, delay + Math.random() * 0.3 * delay);
			};
			agent
				.refreshMe()
				.then((me) => {
					if (stopped) return;
					if (!me.pin.granted) {
						status = 'closed';
						detail = 'Locked - enter the PIN to reconnect';
						onlocked?.();
						return;
					}
					retry();
				})
				.catch(retry);
		};

		ws.onerror = () => ws.close();
	}

	/**
	 * Maps a pointer position to a fraction of the host desktop.
	 *
	 * The element box is NOT the image box. In fullscreen the canvas is stretched
	 * by flex and the picture is letterboxed inside it by object-fit: contain, so
	 * measuring against the element put every click at the wrong place - worse the
	 * further the aspect ratios diverged. Work out where the picture actually sits
	 * and measure against that. getBoundingClientRect already reflects the zoom
	 * and pan transform, so this stays correct while zoomed too.
	 */
	function normalise(e: { clientX: number; clientY: number }) {
		const rect = canvas.getBoundingClientRect();
		const cw = canvas.width || 1;
		const ch = canvas.height || 1;

		const shown = Math.min(rect.width / cw, rect.height / ch);
		const drawnW = cw * shown;
		const drawnH = ch * shown;
		const padX = (rect.width - drawnW) / 2;
		const padY = (rect.height - drawnH) / 2;

		return {
			x: Math.min(1, Math.max(0, (e.clientX - rect.left - padX) / drawnW)),
			y: Math.min(1, Math.max(0, (e.clientY - rect.top - padY) / drawnH))
		};
	}

	// --- gestures --------------------------------------------------------------
	//
	// The spec's touch design: two-finger drag scrolls the HOST, pinch zooms only
	// the CLIENT viewport. A phone has no wheel, so without this there is no way
	// to scroll at all - which is exactly what was missing.

	type Point = { x: number; y: number };
	const pointers = new Map<number, Point>();

	let pressTimer: ReturnType<typeof setTimeout> | null = null;
	let downAt: Point | null = null;
	let downNorm: Point | null = null;
	let downTime = 0;
	let movedFar = false;

	// Hold-and-drag. A finger that rests HOLD_MS and then moves picks things up -
	// an icon, a text handle, a slider, a window - instead of swiping. Resting
	// and lifting without moving is the long press. The sticky Hold button makes
	// the next gesture start held, for when resting still is awkward.
	const HOLD_MS = 500;
	let held = $state(false);
	let holdNext = $state(false);
	let path: Point[] = [];
	let moveStart = 0;

	function addPoint(p: Point) {
		const last = path[path.length - 1];
		if (last && Math.hypot(p.x - last.x, p.y - last.y) < 0.004) return;
		path.push(p);
		// Keep the message small: thin a long path to every other point.
		if (path.length > 96) path = path.filter((_, i) => i % 2 === 0 || i === path.length - 1);
	}

	// Two-finger state. The dominant gesture is decided once and then kept:
	// re-deciding every frame makes a pinch judder into scrolling and back.
	let gesture: 'none' | 'pinch' | 'scroll' = 'none';
	let startSpread = 0;
	let startZoom = 1;
	let lastMidY = 0;
	let scrollCarry = 0;

	const spread = (a: Point, b: Point) => Math.hypot(a.x - b.x, a.y - b.y);
	const midY = (a: Point, b: Point) => (a.y + b.y) / 2;
	const two = () => [...pointers.values()] as [Point, Point];

	function cancelPress() {
		if (pressTimer) clearTimeout(pressTimer);
		pressTimer = null;
	}

	function clampPan() {
		// Keep the picture from being dragged entirely off screen.
		const rect = canvas.getBoundingClientRect();
		const limitX = (Math.max(0, zoom - 1) * rect.width) / 2 / zoom;
		const limitY = (Math.max(0, zoom - 1) * rect.height) / 2 / zoom;
		panX = Math.min(limitX, Math.max(-limitX, panX));
		panY = Math.min(limitY, Math.max(-limitY, panY));
	}

	function pointerDown(e: PointerEvent) {
		canvas.setPointerCapture?.(e.pointerId);
		pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });

		if (pointers.size === 1) {
			downAt = { x: e.clientX, y: e.clientY };
			movedFar = false;
			const p = normalise(e);
			downNorm = p;
			downTime = performance.now();
			held = holdNext;
			path = [p];
			// What a rest means is decided on release: lifted in place, it was a
			// long press (right click on a PC); moved afterwards, a hold-drag.
			pressTimer = setTimeout(() => {
				pressTimer = null;
				held = true;
				navigator.vibrate?.(15);
			}, HOLD_MS);
			return;
		}

		if (pointers.size === 2) {
			cancelPress(); // a second finger is never a click
			const [a, b] = two();
			startSpread = spread(a, b) || 1;
			startZoom = zoom;
			lastMidY = midY(a, b);
			scrollCarry = 0;
			gesture = 'none';
		}
	}

	function pointerMove(e: PointerEvent) {
		const tracked = pointers.get(e.pointerId);
		if (!tracked) return;

		const prev = { x: tracked.x, y: tracked.y };
		tracked.x = e.clientX;
		tracked.y = e.clientY;

		if (pointers.size === 1) {
			if (downAt && !movedFar) {
				const drift = Math.hypot(e.clientX - downAt.x, e.clientY - downAt.y);
				if (drift > 10) {
					movedFar = true;
					moveStart = performance.now();
					cancelPress(); // a drag is not a tap
				}
			}
			if (!movedFar) return;
			// Zoomed in, one finger pans - unless the gesture is a held drag.
			if (zoom > 1 && !held) {
				panX += (e.clientX - prev.x) / zoom;
				panY += (e.clientY - prev.y) / zoom;
				clampPan();
			} else {
				addPoint(normalise(e));
			}
			return;
		}

		if (pointers.size !== 2) return;

		const [a, b] = two();
		const nowSpread = spread(a, b);
		const nowMid = midY(a, b);

		if (gesture === 'none') {
			// Commit to whichever the fingers are actually doing.
			const spreadDelta = Math.abs(nowSpread - startSpread);
			const dragDelta = Math.abs(nowMid - lastMidY);
			if (spreadDelta < 8 && dragDelta < 8) return;
			gesture = spreadDelta > dragDelta ? 'pinch' : 'scroll';
		}

		if (gesture === 'pinch') {
			zoom = Math.min(MAX_ZOOM, Math.max(1, startZoom * (nowSpread / startSpread)));
			if (zoom === 1) {
				panX = 0;
				panY = 0;
			} else {
				clampPan();
			}
			return;
		}

		// Two-finger drag scrolls the host. Carry the remainder so a slow drag
		// still accumulates into a notch instead of being rounded away.
		scrollCarry += nowMid - lastMidY;
		lastMidY = nowMid;
		while (Math.abs(scrollCarry) >= SCROLL_PX_PER_NOTCH) {
			const up = scrollCarry > 0;
			send({ type: 'scroll', delta: up ? WHEEL_NOTCH : -WHEEL_NOTCH });
			scrollCarry += up ? -SCROLL_PX_PER_NOTCH : SCROLL_PX_PER_NOTCH;
		}
	}

	function pointerUp(e: PointerEvent) {
		const wasSingle = pointers.size === 1;
		pointers.delete(e.pointerId);
		if (pointers.size < 2) gesture = 'none';

		if (wasSingle && downNorm) {
			const p = normalise(e);
			const moveMs = Math.min(3000, Math.max(80, Math.round(performance.now() - moveStart)));
			if (!movedFar) {
				// Lifted in place: a tap, or after a rest the long press.
				const button = held ? 'right' : 'left';
				send({ type: 'click', x: p.x, y: p.y, button, down: true });
				send({ type: 'click', x: p.x, y: p.y, button, down: false });
			} else if (held) {
				addPoint(p);
				send({ type: 'drag', points: path.map((q) => [q.x, q.y]), hold: 600, ms: moveMs });
			} else if (zoom <= 1) {
				if (phone) {
					// The drag's own speed is the swipe's speed: a flick flings a
					// list, a slow drag moves it by the distance dragged.
					const ms = Math.min(1500, Math.max(80, Math.round(performance.now() - downTime)));
					send({ type: 'swipe', x1: downNorm.x, y1: downNorm.y, x2: p.x, y2: p.y, ms });
				} else {
					// On a PC a one-finger drag is a mouse drag.
					addPoint(p);
					send({ type: 'drag', points: path.map((q) => [q.x, q.y]), hold: 0, ms: moveMs });
				}
			}
		}
		cancelPress();
		if (wasSingle) {
			held = false;
			holdNext = false;
			path = [];
		}

		if (pointers.size === 0) {
			downAt = null;
			movedFar = false;
		}
	}

	function pointerCancel(e: PointerEvent) {
		pointers.delete(e.pointerId);
		cancelPress();
		if (pointers.size < 2) gesture = 'none';
	}

	function wheel(e: WheelEvent) {
		e.preventDefault();
		send({ type: 'scroll', delta: e.deltaY > 0 ? -WHEEL_NOTCH : WHEEL_NOTCH });
	}

	function scrollBy(notches: number) {
		send({ type: 'scroll', delta: notches * WHEEL_NOTCH });
	}

	function setZoom(next: number) {
		zoom = Math.min(MAX_ZOOM, Math.max(1, Number(next.toFixed(2))));
		if (zoom === 1) {
			panX = 0;
			panY = 0;
		} else {
			clampPan();
		}
	}

	// A permanently visible text field cost a row of screen space and vanished in
	// fullscreen anyway. A button that summons the keyboard is the same capability
	// without the cost.
	let typingOpen = $state(false);
	let typingField = $state<HTMLInputElement | null>(null);

	function openTyping() {
		typingOpen = true;
		// Focus must happen after the element exists for the soft keyboard to show.
		queueMicrotask(() => typingField?.focus());
	}

	function typed(e: Event) {
		const input = e.target as HTMLInputElement;
		if (input.value) {
			send({ type: 'text', text: input.value });
			input.value = '';
		}
	}

	function special(vk: number) {
		send({ type: 'key', key: vk, down: true });
		send({ type: 'key', key: vk, down: false });
	}

	/**
	 * Immersive mode is CSS, not the Fullscreen API.
	 *
	 * The API needs a WebChromeClient with onShowCustomView to work inside the
	 * Android WebView, so relying on it would leave the app's most useful tier
	 * without the feature. Fixed positioning works identically in both, and the
	 * real fullscreen call is made as a best-effort extra where it is available.
	 */
	async function toggleImmersive() {
		immersive = !immersive;

		// Inside the Android shell, CSS fullscreen only fills the WebView - which
		// already IS the content area, so nothing visibly changes. The app has to
		// hide its own chrome and the system bars, and only it can do that.
		const host = (window as unknown as { RemoterHost?: { setFullscreen(on: boolean): void } })
			.RemoterHost;
		host?.setFullscreen(immersive);

		try {
			if (immersive && shell?.requestFullscreen) await shell.requestFullscreen();
			else if (!immersive && document.fullscreenElement) await document.exitFullscreen();
		} catch {
			// Denied or unsupported: the CSS layout already did the useful part.
		}
	}

	onMount(() => {
		connect();

		// Periodic release. Anything still queued after five seconds is stale by
		// definition at these frame rates, and dropping it guarantees nothing is
		// retained across ticks even if a decode stalls.
		const forget = setInterval(() => {
			pending = null;
			frameBytes = frameBytes.filter((f) => performance.now() - f.at < 3000);
		}, 5000);

		const onFsChange = () => {
			if (!document.fullscreenElement && immersive) immersive = false;
		};
		document.addEventListener('fullscreenchange', onFsChange);

		return () => {
			// Tear the pipeline down on exit rather than pausing it: a backgrounded
			// stream is how a data plan disappears.
			stopped = true;
			clearInterval(forget);
			pending = null;
			if (retryTimer) clearTimeout(retryTimer);
			cancelPress();
			pointers.clear();
			document.removeEventListener('fullscreenchange', onFsChange);
			socket?.close();
			socket = null;
		};
	});
</script>

<div class="shell" class:immersive bind:this={shell}>
	<div class="bar">
		<span class="dot" class:live={status === 'live'}></span>
		<span class="muted">{status}{detail ? ` · ${detail}` : ''}</span>
		<span class="spacer"></span>
		<span class="muted" title="rolling 3s average">{kbps} kbps</span>
		{#if dropped > 0}
			<span class="muted" title="frames discarded to keep memory bounded">·{dropped}</span>
		{/if}
		{#if phone && smooth !== 'unsupported'}
			<button
				class="ghost"
				class:active={smooth === 'on'}
				onclick={toggleSmooth}
				title="a screen recording at up to 10 fps instead of screenshots; someone at the phone must tap Start now"
			>
				{smooth === 'requested' ? 'Tap “Start now” on the phone…' : smooth === 'on' ? 'Smooth ✓' : 'Smooth'}
			</button>
		{/if}
		<button class="ghost" class:active={oneToOne} onclick={toggleOneToOne} title="render host pixels at true size">
			1:1
		</button>
		<button class="ghost" onclick={toggleImmersive}>
			{immersive ? 'Exit' : 'Fullscreen'}
		</button>
	</div>

	<div class="viewport" class:immersive class:native={oneToOne}>
		<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
		<canvas
			bind:this={canvas}
			class:immersive
			class:native={oneToOne}
			style:transform={`translate(${panX}px, ${panY}px) scale(${zoom})`}
			onpointerdown={pointerDown}
			onpointermove={pointerMove}
			onpointerup={pointerUp}
			onpointercancel={pointerCancel}
			onwheel={wheel}
		></canvas>
	</div>

	<div class="keys">
		{#if typingOpen}
			<input
				bind:this={typingField}
				class="typing"
				placeholder="typing — keystrokes go to the host"
				oninput={typed}
				onblur={() => (typingOpen = false)}
				autocomplete="off"
				autocapitalize="off"
				spellcheck="false"
			/>
		{:else}
			<button onclick={openTyping}>Type</button>
		{/if}
		{#if phone}
			<button onclick={() => send({ type: 'nav', action: 'back' })} title="Back">◀</button>
			<button onclick={() => send({ type: 'nav', action: 'home' })} title="Home">●</button>
			<button onclick={() => send({ type: 'nav', action: 'recents' })} title="Recent apps">▢</button>
			<button onclick={() => send({ type: 'nav', action: 'notifications' })} title="Notifications">
				Notif
			</button>
			<button
				class:active={holdNext || held}
				onclick={() => (holdNext = !holdNext)}
				title="the next touch starts held: drag to move an icon, select text, or pull a slider"
			>
				Hold
			</button>
			<button onclick={() => special(0x0d)}>Enter</button>
			<button onclick={() => special(0x08)}>Bksp</button>
		{:else}
			<button onclick={() => scrollBy(3)} title="scroll up">▲</button>
			<button onclick={() => scrollBy(-3)} title="scroll down">▼</button>
			<button onclick={() => special(0x0d)}>Enter</button>
			<button onclick={() => special(0x08)}>Bksp</button>
			<button onclick={() => special(0x1b)}>Esc</button>
			<button onclick={() => special(0x09)}>Tab</button>
		{/if}
	</div>

	<div class="zoombar">
		<button onclick={() => setZoom(zoom - 0.25)} disabled={zoom <= 1 || oneToOne}>−</button>
		<span class="muted zoomval">{Math.round(zoom * 100)}%</span>
		<button onclick={() => setZoom(zoom + 0.25)} disabled={zoom >= MAX_ZOOM || oneToOne}>+</button>
		{#if zoom > 1}
			<button class="ghost" onclick={() => setZoom(1)}>Reset</button>
		{/if}
		<span class="spacer"></span>
		<span class="muted hint">
			{oneToOne
				? 'native size · drag the view to scroll'
				: phone
					? 'drag swipes · rest then drag picks up · rest and lift = long press'
					: 'pinch zooms · two fingers scroll'}
		</span>
	</div>

	{#if !immersive}
		<div class="controls">
			<label>
				FPS <span class="muted">{fps}</span>
				<input type="range" min="1" max="30" step="1" bind:value={fps} />
			</label>
			<label>
				Quality <span class="muted">{quality}</span>
				<input type="range" min="15" max="80" step="5" bind:value={quality} />
			</label>
			<label class:disabled={oneToOne}>
				Scale <span class="muted">{oneToOne ? '1.00 (1:1)' : scale.toFixed(2)}</span>
				<input type="range" min="0.2" max="1" step="0.05" bind:value={scale} disabled={oneToOne} />
			</label>
		</div>

		<p class="muted note">
			Tap to click, long-press for right click, drag to pan when zoomed. Zoom is local to this
			screen — the host is never resized. An idle desktop sends nothing at all.
			{#if oneToOne}
				<br />1:1 sends the desktop at full resolution — several times the bandwidth of a
				scaled frame. Worth it from another computer, not from a phone.
			{/if}
			{#if fps > 17}
				<br />Above ~17 fps this host cannot capture fast enough; frames arrive as quickly as
				it can produce them, and bandwidth rises steeply.
			{:else if fps > 10}
				<br />Above 10 fps a faster, coarser downscale is used — text gets slightly softer.
			{/if}
		</p>
	{/if}
</div>

<style>
	.shell.immersive {
		position: fixed;
		inset: 0;
		z-index: 50;
		display: flex;
		flex-direction: column;
		background: #000;
		padding: env(safe-area-inset-top) 0.5rem env(safe-area-inset-bottom);
	}

	.bar {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.78rem;
		margin-bottom: 0.4rem;
	}
	.spacer { flex: 1; }
	.muted { color: var(--muted); }

	.dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: var(--err);
	}
	.dot.live { background: var(--ok); }

	.ghost.active {
		border-color: var(--accent);
		color: var(--accent);
	}

	.ghost {
		background: transparent;
		border: 1px solid var(--line);
		border-radius: 6px;
		color: var(--text);
		min-height: 32px;
		padding: 0 0.6rem;
		font-size: 0.75rem;
	}

	/* Clips the zoomed canvas so panning reveals rather than overflows. */
	/* Native size: the picture is bigger than the viewport, so let it scroll. */
	.viewport.native {
		overflow: auto;
	}
	.viewport.native canvas {
		width: auto;
		max-width: none;
		transform: none !important;
	}

	.viewport {
		overflow: hidden;
		border: 1px solid var(--line);
		border-radius: 8px;
		background: #000;
	}
	.viewport.immersive {
		flex: 1;
		min-height: 0;
		display: flex;
		border: 0;
		border-radius: 0;
	}

	canvas {
		width: 100%;
		display: block;
		background: #000;
		/* We own every gesture here; the browser must not pan or zoom the page. */
		touch-action: none;
		transform-origin: center center;
	}

	canvas.immersive {
		flex: 1;
		min-height: 0;
		height: auto;
		object-fit: contain;
	}

	.keys,
	.zoombar {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		margin-top: 0.5rem;
		overflow-x: auto;
		flex: none;
	}
	.keys button,
	.zoombar button {
		flex: none;
		min-width: 48px;
		min-height: 40px;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 8px;
		font-size: 0.8rem;
	}
	.keys button:disabled,
	.zoombar button:disabled { opacity: 0.4; }
	/* Hold armed, or a press that has rested long enough to pick up. */
	.keys button.active {
		border-color: var(--accent);
		color: var(--accent);
	}

	.zoombar .ghost { min-width: 0; }
	.zoomval { min-width: 44px; text-align: center; font-size: 0.8rem; }
	.hint { font-size: 0.7rem; white-space: nowrap; }

	.typing {
		flex: 1;
		min-width: 100px;
		min-height: 40px;
		padding: 0 0.6rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 8px;
		color: var(--text);
		font: inherit;
	}

	.controls {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		margin-top: 0.75rem;
	}
	.controls label {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.82rem;
	}
	.controls input[type='range'] { flex: 1; accent-color: var(--accent); }
	.controls label.disabled { opacity: 0.5; }

	.note { font-size: 0.76rem; margin-top: 0.6rem; }
</style>
