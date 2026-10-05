<script lang="ts">
	import { onMount } from 'svelte';
	import {
		agent,
		ConfirmNeeded,
		PinNeeded,
		Pending,
		type ActionView,
		type Job,
		type Glance
	} from '$lib/agent.svelte';
	import PinDialog from '$lib/PinDialog.svelte';

	type Tier = 'actions' | 'glance' | 'console' | 'live';

	let selected = $state<string | null>(null);
	let pending = $state<{ action: ActionView; nonce: string } | null>(null);
	let busy = $state<string | null>(null);
	let tier = $state<Tier>('actions');
	let loading = $state(true);
	let waitingAccess = $state(0);

	// The PIN prompt, and what to do once it is satisfied.
	let pinPrompt = $state<{ mode: 'enter' | 'set'; reason: string; then: () => void } | null>(null);

	// When hosted inside the Android app the surrounding UI is native, so the
	// web layer renders only the tier (and device) it was asked for.
	const params = new URLSearchParams(typeof location !== 'undefined' ? location.search : '');
	const embedded = params.has('embed');

	// Tier 1.5 controls. Defaults land ~60 KB on a 2560x1600 panel - the
	// cheapest useful answer to "is something waiting?".
	let monitor = $state(0);
	let quality = $state(60);
	let scale = $state(0.5);
	let frame = $state<Glance | null>(null);
	let loadingFrame = $state(false);
	let glanceError = $state('');

	let armHours = $state(0);

	function askPin(err: PinNeeded, then: () => void) {
		pinPrompt = { mode: err.unset ? 'set' : 'enter', reason: err.unset ? err.message : '', then };
	}

	async function refreshGlance() {
		loadingFrame = true;
		glanceError = '';
		try {
			const next = await agent.glance(monitor, quality, scale);
			// Release the previous blob; leaking these is how a long session
			// quietly eats memory on a phone.
			if (frame) URL.revokeObjectURL(frame.url);
			frame = next;
		} catch (e) {
			if (e instanceof PinNeeded) askPin(e, refreshGlance);
			else glanceError = e instanceof Error ? e.message : String(e);
		} finally {
			loadingFrame = false;
		}
	}

	// xterm.js is ~330 KB - more than three times the rest of the app. Loading it
	// for someone who only ever taps Tier 1 buttons would contradict the entire
	// premise, so the terminal is fetched the first time Console is opened.
	let ConsoleView = $state<typeof import('$lib/Console.svelte').default | null>(null);
	let consoleLoading = $state(false);

	async function loadConsole() {
		if (ConsoleView || consoleLoading) return;
		consoleLoading = true;
		try {
			ConsoleView = (await import('$lib/Console.svelte')).default;
			loadedFine();
		} catch (e) {
			staleBuild(e);
		} finally {
			consoleLoading = false;
		}
	}

	// A page opened before a deploy asks for its lazy chunks by their old names.
	// Reload once to pick up the new build instead of showing a module error;
	// the flag stops a genuinely broken deploy from reloading forever.
	function staleBuild(e: unknown) {
		const msg = e instanceof Error ? e.message : String(e);
		const stale = /dynamically imported module|Importing a module script failed|error loading dynamically/i.test(msg);
		let reloaded = false;
		try {
			reloaded = sessionStorage.getItem('remoter.reloaded') === '1';
			if (stale && !reloaded) sessionStorage.setItem('remoter.reloaded', '1');
		} catch {
			/* storage unavailable: just report */
		}
		if (stale && !reloaded) {
			location.reload();
			return;
		}
		agent.error = msg;
	}

	// A lazy chunk loaded: the build is good, so a future stale-build error may
	// reload again.
	function loadedFine() {
		try {
			sessionStorage.removeItem('remoter.reloaded');
		} catch {
			/* ignore */
		}
	}

	// Live carries its own renderer and JPEG decoding; keep it out of the initial
	// bundle the same way the terminal is kept out.
	let LiveView = $state<typeof import('$lib/Live.svelte').default | null>(null);

	async function loadLive() {
		if (LiveView) return;
		try {
			LiveView = (await import('$lib/Live.svelte')).default;
			loadedFine();
		} catch (e) {
			staleBuild(e);
		}
	}

	// Remounts Live/Console after a PIN re-entry, so they reconnect.
	let unlockCount = $state(0);

	async function toTier(next: Tier) {
		if (next !== 'actions') {
			// Asked up front: a refused WebSocket handshake arrives with no reason.
			try {
				await agent.requirePin();
			} catch (e) {
				if (e instanceof PinNeeded) return askPin(e, () => toTier(next));
				agent.error = e instanceof Error ? e.message : String(e);
				return;
			}
		}
		tier = next;
		if (next === 'console') loadConsole();
		if (next === 'live') loadLive();
		if (next === 'glance' && !frame && agent.health?.tiers.glance) refreshGlance();
	}

	function relock() {
		askPin(new PinNeeded('', false), () => unlockCount++);
	}

	async function pick(id: string) {
		if (id === agent.device) return;
		if (frame) URL.revokeObjectURL(frame.url);
		frame = null;
		selected = null;
		tier = 'actions';
		await agent.select(id);
	}

	const selectedJob = $derived(agent.jobs.find((j) => j.id === selected) ?? null);
	const selectedLog = $derived(selected ? (agent.logs[selected] ?? []) : []);
	const dev = $derived(agent.current);
	const tiers = $derived(dev?.online ? dev.health?.tiers : undefined);

	onMount(() => {
		let alive = true;
		let timer: ReturnType<typeof setTimeout>;

		const boot = async () => {
			if (embedded) {
				const d = params.get('device');
				if (d) agent.device = d;
			}
			await agent.start();
			loading = false;

			const wanted = params.get('tier');
			if (embedded && (wanted === 'console' || wanted === 'glance' || wanted === 'live')) {
				await toTier(wanted);
			}
			if (embedded) history.replaceState(null, '', location.pathname + location.search);
			schedule();
		};

		// While waiting for approval, ask every few seconds; once in, keep the
		// device list fresh - but only while the page is visible, because a
		// backgrounded tab polling on mobile data is exactly the waste the tiers
		// exist to avoid.
		const schedule = () => {
			if (!alive) return;
			// Faster while the selected device is away: it is most likely
			// rebooting, and the moment it is back is the moment that matters.
			const away = agent.current !== null && !agent.current.online;
			timer = setTimeout(tick, !agent.approved ? 5_000 : away ? 3_000 : 15_000);
		};
		const tick = async () => {
			if (document.visibilityState === 'visible') {
				try {
					if (!agent.approved) {
						await agent.refreshMe();
						if (agent.approved) await agent.start();
					} else {
						await agent.refreshDevices();
						if (!embedded) refreshAccess();
						if (agent.current?.online && !agent.connected && agent.actions.length === 0) {
							await agent.select(agent.device);
						}
					}
				} catch (e) {
					if (!(e instanceof Pending)) agent.error = e instanceof Error ? e.message : String(e);
				}
			}
			schedule();
		};

		boot().then(() => {
			if (!embedded) refreshAccess();
		});
		return () => {
			alive = false;
			clearTimeout(timer);
			agent.disconnect();
		};
	});

	async function refreshAccess() {
		try {
			const a = await agent.access();
			waitingAccess = [...a.controllers, ...a.nodes].filter((d) => d.state === 'pending').length;
		} catch {
			/* not fatal: just no badge */
		}
	}

	async function run(action: ActionView, nonce?: string) {
		busy = action.id;
		try {
			const jobId = await agent.run(action.id, nonce);
			pending = null;
			selected = jobId;
		} catch (e) {
			if (e instanceof ConfirmNeeded) pending = { action, nonce: e.nonce };
			else agent.error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = null;
		}
	}

	async function open(job: Job) {
		selected = job.id;
		if (!agent.logs[job.id]) await agent.tail(job.id).catch(() => {});
	}

	async function arm() {
		try {
			await agent.arm(armHours);
		} catch (e) {
			agent.error = e instanceof Error ? e.message : String(e);
		}
	}

	async function disarm() {
		try {
			await agent.disarm();
		} catch (e) {
			agent.error = e instanceof Error ? e.message : String(e);
		}
	}

	// Socket events are the fast path, not the only path. A job that finished
	// before its events arrived - or during a dropped connection - would
	// otherwise render as empty forever. Fetch the retained tail once per job.
	const fetched = new Set<string>();
	$effect(() => {
		const job = selectedJob;
		if (!job || !isTerminal(job.state) || fetched.has(job.id)) return;
		if (agent.logs[job.id]?.length) return;

		fetched.add(job.id);
		agent.tail(job.id).catch(() => {});
	});

	function isTerminal(s: Job['state']) {
		return s === 'done' || s === 'failed' || s === 'cancelled' || s === 'timeout';
	}

	function stateColor(s: Job['state']) {
		if (s === 'done') return 'var(--ok)';
		if (s === 'running' || s === 'queued') return 'var(--accent)';
		if (s === 'cancelled') return 'var(--muted)';
		return 'var(--err)';
	}

	function kb(n: number) {
		if (n < 1024) return `${n} B`;
		if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
		return `${(n / 1048576).toFixed(1)} MB`;
	}

	function clock(iso: string) {
		return new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
	}

	function ago(iso?: string) {
		if (!iso) return 'never';
		const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
		if (s < 90) return 'just now';
		if (s < 5400) return `${Math.round(s / 60)} min ago`;
		if (s < 129600) return `${Math.round(s / 3600)} h ago`;
		return new Date(iso).toLocaleDateString();
	}

	function until(iso: string) {
		const d = new Date(iso);
		const sameDay = d.toDateString() === new Date().toDateString();
		return sameDay
			? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
			: d.toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' });
	}

	const tierNames: Record<string, string> = { glance: 'Glance', live: 'Live', console: 'Console' };
</script>

<svelte:head><title>Remoter{dev ? ` · ${dev.name}` : ''}</title></svelte:head>

{#if loading}
	<main class="center"><p class="muted">Connecting to the hub…</p></main>
{:else if agent.me && !agent.approved}
	<main class="center">
		<h1>Remoter</h1>
		<p>
			<strong>{agent.me.device.name}</strong> is waiting for approval.
		</p>
		<p class="muted">
			Approve it from a device you already use (Access page), or on the hub:
		</p>
		<pre>sudo remoter-hub approve {agent.me.device.name}</pre>
		<p class="muted small">This page continues on its own once you are approved.</p>
	</main>
{:else if !agent.me}
	<main class="center">
		<h1>Remoter</h1>
		<p class="err">{agent.error || 'Cannot reach the hub.'}</p>
		<p class="muted">The hub only answers devices on the tailnet. Is Tailscale on?</p>
		<button class="primary" onclick={() => location.reload()}>Retry</button>
	</main>
{:else}
	{#if !embedded}
		<header>
			<div
				class="dot"
				class:live={agent.connected}
				title={agent.connected ? 'connected' : 'not connected'}
			></div>
			<strong>Remoter</strong>
			<span class="spacer"></span>
			<span class="muted" title="Data used this session">{kb(agent.bytes)}</span>
			<a class="link" href="/access">
				access{#if waitingAccess}<span class="count">{waitingAccess}</span>{/if}
			</a>
		</header>

		<nav class="devices" aria-label="Devices">
			{#each agent.devices as d (d.id)}
				<button class:on={d.id === agent.device} onclick={() => pick(d.id)}>
					<span class="dot" class:live={d.online}></span>
					{d.name}
					{#if d.health?.arm?.armed}<span class="armed" title="armed">●</span>{/if}
				</button>
			{:else}
				<p class="muted small">
					No devices yet. Install the node on a computer (Access → Add a computer), then approve it.
				</p>
			{/each}
		</nav>
	{/if}

	<main class:embedded>
		{#if agent.error}
			<p class="err">{agent.error}</p>
		{/if}

		{#if dev && !embedded}
			<section class="card">
				<div class="drow">
					<strong>{dev.name}</strong>
					<span class="muted">{dev.os}</span>
					<span class="spacer"></span>
					{#if dev.online}
						<span class="ok">online</span>
					{:else}
						<span class="err">offline · seen {ago(dev.lastSeen)}</span>
					{/if}
				</div>
				{#if dev.online && dev.health}
					<div class="facts muted">
						{#if dev.os !== 'android'}
							<span>{dev.health.user ? `signed in: ${dev.health.user}` : 'nobody signed in'}</span>
						{/if}
						{#if dev.health.power?.hasBattery}
							<span>
								battery {dev.health.power.percent}%{dev.health.power.onAC ? ' · on AC' : ''}
							</span>
						{/if}
					</div>
					{#if dev.sessions.length}
						<p class="sessions">
							Connected:
							{dev.sessions.map((s) => `${s.controller} (${tierNames[s.tier] ?? s.tier})`).join(', ')}
						</p>
					{/if}
					{#if dev.health.arm?.supported}
						<div class="arm">
							{#if dev.health.arm.armed}
								<span class="armed-label">
									Armed{dev.health.arm.until ? ` until ${until(dev.health.arm.until)}` : ''}
								</span>
								<span class="spacer"></span>
								<button onclick={disarm}>Disarm</button>
							{:else}
								<select bind:value={armHours} aria-label="Arm for">
									<option value={0}>until disarmed</option>
									<option value={2}>for 2 hours</option>
									<option value={8}>for 8 hours</option>
									<option value={24}>for 1 day</option>
									<option value={72}>for 3 days</option>
									<option value={168}>for 1 week</option>
								</select>
								<button onclick={arm}>Arm</button>
							{/if}
						</div>
						<p class="muted small">
							{#if dev.health.arm.armed}
								Stays awake with the lid closed. Disarms itself on low battery, or when someone uses
								it after it was left alone for 10 minutes.
							{:else if dev.health.arm.lastDisarm && dev.health.arm.lastDisarm.reason !== 'manual'}
								Disarmed itself {ago(dev.health.arm.lastDisarm.at)}: {dev.health.arm.lastDisarm
									.reason}.
							{:else}
								Arm before leaving: a sleeping laptop cannot be woken over Wi-Fi.
							{/if}
						</p>
						{#if dev.health.arm.error}<p class="err small">{dev.health.arm.error}</p>{/if}
					{/if}
				{/if}
			</section>
		{/if}

		{#if !dev}
			<!-- nothing selected: the device list says why -->
		{:else if !dev.online}
			<p class="muted">
				{dev.name} is not answering. If it is asleep it cannot be woken remotely - arm it before
				leaving next time.
			</p>
		{:else}
			{#if tier === 'glance'}
				<h2>Glance</h2>
				{#if !tiers?.glance}
					<p class="err">Cannot capture a screen. {tiers?.glanceReason ?? ''}</p>
				{:else}
					<div class="controls">
						{#if agent.monitors.length > 1}
							<label>
								Monitor
								<select bind:value={monitor}>
									{#each agent.monitors as m (m.index)}
										<option value={m.index}>{m.index}: {m.width}×{m.height}</option>
									{/each}
								</select>
							</label>
						{/if}
						<label>
							Quality <span class="muted">{quality}</span>
							<input type="range" min="20" max="90" step="5" bind:value={quality} />
						</label>
						<label>
							Scale <span class="muted">{scale.toFixed(2)}</span>
							<input type="range" min="0.2" max="1" step="0.05" bind:value={scale} />
						</label>
					</div>

					<button class="primary" onclick={refreshGlance} disabled={loadingFrame}>
						{loadingFrame ? 'Capturing…' : frame ? 'Refresh' : 'Capture'}
					</button>

					{#if glanceError}<p class="err">{glanceError}</p>{/if}

					{#if frame}
						<figure>
							<img src={frame.url} alt="Screen of {dev.name}" />
							<figcaption class="muted">
								{frame.dimensions} · {kb(frame.bytes)} · {frame.at.toLocaleTimeString()}
							</figcaption>
						</figure>
					{:else if !loadingFrame}
						<p class="muted">No frame yet. One capture costs roughly 50–90 KB at these settings.</p>
					{/if}
				{/if}
			{/if}

			{#if tier === 'console'}
				<h2>Console</h2>
				{#if !tiers?.console}
					<p class="err">{tiers?.consoleReason ?? 'Console is unavailable.'}</p>
				{:else}
					<!-- Keyed on device and unlocks so a switch gets a fresh shell
					     rather than a dead socket. -->
					{#if ConsoleView}
						{#key `${agent.device}:${unlockCount}`}
							<ConsoleView onbytes={(n) => (agent.bytes += n)} />
						{/key}
					{:else}
						<p class="muted">Loading terminal…</p>
					{/if}
					<p class="muted small">
						A full shell as {dev.health?.user || 'the signed-in user'}, unlike the Tier 1 action
						whitelist. Leaving this tab ends the session.
					</p>
				{/if}
			{/if}

			{#if tier === 'live'}
				<h2>Live</h2>
				{#if !tiers?.live}
					<p class="err">{tiers?.glanceReason ?? 'Live is unavailable on this device.'}</p>
				{:else if LiveView}
					{#key `${agent.device}:${unlockCount}`}
						<LiveView onbytes={(n) => (agent.bytes += n)} onlocked={relock} os={dev.os} />
					{/key}
				{:else}
					<p class="muted">Loading…</p>
				{/if}
			{/if}

			{#if tier === 'actions'}
				<h2>Actions</h2>
				{#if !tiers?.actions}
					<p class="muted">{tiers?.actionsReason ?? 'Actions are unavailable.'}</p>
				{:else}
					<div class="grid">
						{#each agent.actions as action (action.id)}
							<button
								class="action"
								disabled={!action.available || busy === action.id}
								title={action.reason}
								onclick={() => run(action)}
							>
								<span class="label">{action.label}</span>
								{#if action.confirm}<span class="badge">confirm</span>{/if}
								{#if !action.available}<span class="reason">{action.reason}</span>{/if}
							</button>
						{:else}
							<p class="muted small">No actions defined on {dev.name} (actions.yaml).</p>
						{/each}
					</div>

					<h2>Jobs</h2>
					{#if agent.jobs.length === 0}
						<p class="muted">Nothing has run yet.</p>
					{/if}
					<ul class="jobs">
						{#each agent.jobs as job (job.id)}
							<li>
								<button class="job" onclick={() => open(job)} class:sel={selected === job.id}>
									<span class="state" style:background={stateColor(job.state)}></span>
									<span class="jlabel">{job.label}</span>
									<span class="muted">{clock(job.started)}</span>
								</button>
							</li>
						{/each}
					</ul>

					{#if selectedJob}
						<section class="detail">
							<div class="drow">
								<strong>{selectedJob.label}</strong>
								<span class="spacer"></span>
								<span style:color={stateColor(selectedJob.state)}>{selectedJob.state}</span>
								{#if selectedJob.exit !== undefined}<span class="muted">exit {selectedJob.exit}</span
									>{/if}
							</div>
							{#if selectedJob.error}<p class="err">{selectedJob.error}</p>{/if}
							<!-- Distinguish "produced nothing" from "nothing yet": a successful
							     `go build` is silent, and that should not look like a failure. -->
							<pre>{selectedLog.join('\n') ||
									(isTerminal(selectedJob.state)
										? 'This action produced no output.'
										: 'Waiting for output…')}</pre>
							{#if selectedJob.state === 'running' || selectedJob.state === 'queued'}
								<button class="danger" onclick={() => agent.cancel(selectedJob.id)}>Cancel</button>
							{/if}
						</section>
					{/if}
				{/if}
			{/if}
		{/if}
	</main>

	<!--
		The mode switcher, not the streaming, is the product. It gets permanent
		screen real estate, and the Tier 1 socket stays connected across every
		switch - changing tier changes what is rendered, not what is connected.
	-->
	{#if !embedded}
		<nav class="tiers">
			<button class:on={tier === 'actions'} onclick={() => toTier('actions')}>
				<span class="tname">1 · Actions</span>
				<span class="tcost">~KB</span>
			</button>
			<button
				class:on={tier === 'glance'}
				disabled={!tiers?.glance}
				onclick={() => toTier('glance')}
			>
				<span class="tname">1.5 · Glance</span>
				<span class="tcost">~50 KB</span>
			</button>
			<button
				class:on={tier === 'console'}
				disabled={!tiers?.console}
				onclick={() => toTier('console')}
			>
				<span class="tname">2 · Console</span>
				<span class="tcost">KB/s</span>
			</button>
			<button class:on={tier === 'live'} disabled={!tiers?.live} onclick={() => toTier('live')}>
				<span class="tname">3 · Live</span>
				<span class="tcost">~64 kbps</span>
			</button>
		</nav>
	{/if}
{/if}

{#if pending}
	<div class="scrim">
		<div class="dialog">
			<h3>{pending.action.label}</h3>
			<p class="muted">This action asks for confirmation. Run it on {dev?.name}?</p>
			<div class="drow">
				<button onclick={() => (pending = null)}>Cancel</button>
				<span class="spacer"></span>
				<button class="danger" onclick={() => run(pending!.action, pending!.nonce)}>Run</button>
			</div>
		</div>
	</div>
{/if}

{#if pinPrompt}
	<PinDialog
		mode={pinPrompt.mode}
		reason={pinPrompt.reason}
		ondone={() => {
			const then = pinPrompt?.then;
			pinPrompt = null;
			then?.();
		}}
		oncancel={() => (pinPrompt = null)}
	/>
{/if}

<style>
	main {
		max-width: 640px;
		margin: 0 auto;
		/* Bottom padding clears the fixed tier switcher. */
		padding: 0.75rem 1rem 6rem;
	}

	/* Embedded in the Android shell: native chrome replaces the web chrome. */
	main.embedded {
		padding: 0.5rem 0.75rem 1rem;
	}

	main.center {
		max-width: 440px;
		margin: 12vh auto;
		padding: 1.5rem;
	}
	main.center pre {
		margin: 0.5rem 0;
		user-select: all;
	}

	.devices {
		display: flex;
		gap: 0.5rem;
		overflow-x: auto;
		padding: 0.6rem 1rem;
		border-bottom: 1px solid var(--line);
		scrollbar-width: none;
	}
	.devices button {
		display: flex;
		align-items: center;
		gap: 0.45rem;
		flex: none;
		min-height: 40px;
		padding: 0 0.85rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 999px;
		color: var(--muted);
	}
	.devices button.on {
		color: var(--text);
		border-color: var(--accent);
	}
	.armed {
		color: var(--warn);
		font-size: 0.7rem;
	}

	.card {
		margin-top: 0.5rem;
		padding: 0.85rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 10px;
	}
	.facts {
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem 1rem;
		margin-top: 0.35rem;
		font-size: 0.82rem;
	}
	.sessions {
		margin: 0.5rem 0 0;
		font-size: 0.82rem;
		color: var(--warn);
	}
	.arm {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		margin-top: 0.75rem;
	}
	.arm select,
	.controls select {
		background: var(--bg);
		color: var(--text);
		border: 1px solid var(--line);
		border-radius: 6px;
		min-height: 40px;
		padding: 0 0.5rem;
		font: inherit;
	}
	.arm button {
		min-height: 40px;
		padding: 0 1rem;
		background: transparent;
		border: 1px solid var(--accent);
		border-radius: 8px;
		color: var(--accent);
	}
	.armed-label {
		color: var(--warn);
		font-weight: 600;
	}
	.ok {
		color: var(--ok);
	}
	.count {
		display: inline-block;
		margin-left: 0.3rem;
		min-width: 1.2rem;
		padding: 0 0.3rem;
		border-radius: 999px;
		background: var(--warn);
		color: #000;
		font-size: 0.7rem;
		text-align: center;
		text-decoration: none;
	}

	.tiers {
		position: fixed;
		bottom: 0;
		left: 0;
		right: 0;
		display: grid;
		grid-template-columns: repeat(4, 1fr);
		background: var(--surface);
		border-top: 1px solid var(--line);
		padding-bottom: env(safe-area-inset-bottom);
	}

	.tiers button {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: 0.15rem;
		min-height: 56px;
		padding: 0.5rem 0.25rem;
		background: none;
		border: 0;
		border-top: 2px solid transparent;
		color: var(--muted);
	}
	.tiers button.on {
		color: var(--text);
		border-top-color: var(--accent);
	}
	.tiers button:disabled {
		opacity: 0.35;
	}
	.tname {
		font-size: 0.82rem;
		font-weight: 600;
	}
	.tcost {
		font-size: 0.68rem;
		opacity: 0.8;
	}

	.controls {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		margin-bottom: 0.9rem;
	}
	.controls label {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.85rem;
	}
	.controls input[type='range'] {
		flex: 1;
		accent-color: var(--accent);
	}

	button.primary {
		width: 100%;
		min-height: 48px;
		background: var(--accent);
		color: #06121d;
		border: 0;
		border-radius: 8px;
		font-weight: 600;
	}
	button.primary:disabled {
		opacity: 0.6;
	}

	figure {
		margin: 1rem 0 0;
	}
	figure img {
		width: 100%;
		border: 1px solid var(--line);
		border-radius: 8px;
		display: block;
	}
	figcaption {
		font-size: 0.78rem;
		margin-top: 0.4rem;
		text-align: center;
	}

	.small {
		font-size: 0.78rem;
		margin-top: 0.6rem;
	}

	header {
		position: sticky;
		top: 0;
		z-index: 5;
		display: flex;
		align-items: center;
		gap: 0.6rem;
		padding: 0.75rem 1rem;
		background: var(--surface);
		border-bottom: 1px solid var(--line);
	}

	.spacer {
		flex: 1;
	}
	.muted {
		color: var(--muted);
	}
	.err {
		color: var(--err);
	}

	.dot {
		width: 9px;
		height: 9px;
		flex: none;
		border-radius: 50%;
		background: var(--err);
	}
	.dot.live {
		background: var(--ok);
	}

	h2 {
		font-size: 0.8rem;
		text-transform: uppercase;
		letter-spacing: 0.08em;
		color: var(--muted);
		margin: 1.5rem 0 0.6rem;
	}

	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
		gap: 0.6rem;
	}

	.action {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		/* Thumb-sized: this is used one-handed on a phone. */
		min-height: 64px;
		padding: 0.85rem;
		text-align: left;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 10px;
	}
	.action:active:not(:disabled) {
		background: var(--surface-2);
	}
	.action:disabled {
		opacity: 0.45;
		cursor: not-allowed;
	}
	.label {
		font-weight: 600;
	}

	.badge {
		align-self: flex-start;
		font-size: 0.7rem;
		color: var(--warn);
		border: 1px solid var(--warn);
		border-radius: 999px;
		padding: 0 0.4rem;
	}
	.reason {
		font-size: 0.75rem;
		color: var(--muted);
	}

	.jobs {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.job {
		display: flex;
		align-items: center;
		gap: 0.6rem;
		width: 100%;
		min-height: 48px;
		padding: 0.6rem 0.75rem;
		background: transparent;
		border: 0;
		border-bottom: 1px solid var(--line);
		text-align: left;
	}
	.job.sel {
		background: var(--surface);
	}
	.jlabel {
		flex: 1;
	}

	.state {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		flex: none;
	}

	.detail {
		margin-top: 1rem;
		padding: 0.85rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 10px;
	}

	.drow {
		display: flex;
		align-items: center;
		gap: 0.6rem;
	}

	pre {
		max-height: 45vh;
		margin: 0.75rem 0;
		padding: 0.6rem;
		overflow: auto;
		background: var(--bg);
		border-radius: 6px;
		font-size: 0.8rem;
		white-space: pre-wrap;
		word-break: break-word;
	}

	button.danger {
		background: transparent;
		border: 1px solid var(--err);
		color: var(--err);
		border-radius: 8px;
		padding: 0.6rem 1rem;
		min-height: 44px;
	}

	a.link {
		color: var(--muted);
		text-decoration: underline;
		font-size: 0.8rem;
	}

	.scrim {
		position: fixed;
		inset: 0;
		display: grid;
		place-items: center;
		padding: 1rem;
		background: rgba(0, 0, 0, 0.6);
	}

	.dialog {
		width: 100%;
		max-width: 340px;
		padding: 1.25rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 12px;
	}
	.dialog h3 {
		margin: 0 0 0.5rem;
	}
	.dialog button {
		min-height: 44px;
		padding: 0 1rem;
		background: transparent;
		border: 1px solid var(--line);
		border-radius: 8px;
	}
</style>
