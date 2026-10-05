<script lang="ts">
	// Access: who may use the hub, and which computers it may reach.
	//
	// Approving is the most powerful thing this UI can do - an approved
	// controller can reach every screen and shell - so the hub asks for the PIN
	// before it, and before revoking or reading the activity log.
	import { onMount } from 'svelte';
	import {
		agent,
		PinNeeded,
		type Access,
		type AccessEntry,
		type AuditEntry
	} from '$lib/agent.svelte';
	import PinDialog from '$lib/PinDialog.svelte';

	let access = $state<Access | null>(null);
	let audit = $state<AuditEntry[] | null>(null);
	let error = $state('');
	let apk = $state<number | null>(null);
	let pinPrompt = $state<{ mode: 'enter' | 'set' | 'change'; reason: string; then?: () => void } | null>(
		null
	);

	const waiting = $derived(
		access
			? [
					...access.controllers.filter((c) => c.state === 'pending').map((c) => ({ ...c, kind: 'controller' as const })),
					...access.nodes.filter((n) => n.state === 'pending').map((n) => ({ ...n, kind: 'node' as const }))
				]
			: []
	);
	const controllers = $derived(access?.controllers.filter((c) => c.state === 'approved') ?? []);
	const nodes = $derived(access?.nodes.filter((n) => n.state === 'approved') ?? []);
	const hubUrl = typeof location !== 'undefined' ? location.origin : '';

	onMount(async () => {
		try {
			await agent.refreshMe();
			if (!agent.approved) {
				location.href = '/';
				return;
			}
			access = await agent.access();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
		try {
			const res = await fetch('/dl/remoter.apk', { method: 'HEAD' });
			if (res.ok) apk = Number(res.headers.get('Content-Length') ?? 0);
		} catch {
			/* no APK published */
		}
	});

	/** Runs a PIN-guarded operation, asking for the PIN once if needed. */
	async function guarded(op: () => Promise<void>) {
		error = '';
		try {
			await op();
		} catch (e) {
			if (e instanceof PinNeeded) {
				pinPrompt = { mode: e.unset ? 'set' : 'enter', reason: '', then: () => guarded(op) };
			} else {
				error = e instanceof Error ? e.message : String(e);
			}
		}
	}

	const approve = (d: AccessEntry & { kind: 'controller' | 'node' }) =>
		guarded(async () => {
			access = await agent.approve(d.kind, d.id);
		});

	function remove(kind: 'controller' | 'node', d: AccessEntry) {
		const what =
			d.state === 'pending'
				? `Reject ${d.name}?`
				: kind === 'controller'
					? `Revoke ${d.name}? It loses access to every device, and its open sessions end now.`
					: `Remove ${d.name}? It will no longer be reachable through the hub.`;
		if (!confirm(what)) return;
		guarded(async () => {
			access = await agent.removeAccess(kind, d.id);
		});
	}

	const loadAudit = () =>
		guarded(async () => {
			audit = await agent.audit(150);
		});

	async function lockNow() {
		await agent.lock();
		audit = null;
	}

	function when(iso: string) {
		return new Date(iso).toLocaleString([], {
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	const events: Record<string, string> = {
		'session.open': 'opened',
		'session.close': 'closed',
		'access.request': 'asked for access',
		'access.approve': 'approved',
		'access.remove': 'removed',
		'action.run': 'ran action',
		'device.arm': 'armed',
		'device.disarm': 'disarmed',
		'device.offline': 'went offline',
		'device.online': 'came back online',
		'pin.ok': 'entered the PIN',
		'pin.wrong': 'entered a wrong PIN',
		'pin.locked': 'was locked out (wrong PINs)',
		'pin.set': 'set the PIN',
		'pin.clear': 'cleared the PIN'
	};

	function describe(e: AuditEntry) {
		const verb = events[e.event] ?? e.event;
		const parts = [e.controller, verb];
		if (e.tier) parts.push(e.tier[0].toUpperCase() + e.tier.slice(1));
		if (e.device && e.event !== 'device.offline' && e.event !== 'device.online') {
			parts.push(e.event.startsWith('access') ? e.device : `on ${e.device}`);
		} else if (e.device) {
			parts.unshift(e.device);
		}
		if (e.detail && (e.event === 'action.run' || e.event === 'session.close')) parts.push(`(${e.detail})`);
		return parts.filter(Boolean).join(' ');
	}
</script>

<svelte:head><title>Remoter · Access</title></svelte:head>

<header>
	<a href="/" class="back">← Back</a>
	<strong>Access</strong>
	<span class="spacer"></span>
	{#if agent.me?.pin.granted}
		<button class="link" onclick={lockNow}>lock</button>
	{/if}
</header>

<main>
	{#if error}<p class="err">{error}</p>{/if}

	{#if waiting.length}
		<section>
			<h2>Waiting for approval</h2>
			{#each waiting as d (d.kind + d.id)}
				<div class="row">
					<div class="who">
						<strong>{d.name}</strong>
						<span class="muted small">
							{d.kind === 'controller' ? 'wants to control devices' : 'wants to be controllable'}
							· {d.os}
						</span>
					</div>
					<button class="ghost" onclick={() => remove(d.kind, d)}>Reject</button>
					<button class="go" onclick={() => approve(d)}>Approve</button>
				</div>
			{/each}
			<p class="muted small">
				Only approve devices you recognise: an approved controller can reach every computer here.
			</p>
		</section>
	{/if}

	<section>
		<h2>PIN</h2>
		{#if agent.me?.pin.set}
			<p class="muted">
				Set. Needed for Glance, Live, Console and approvals; stays unlocked for 5 minutes after
				use.
			</p>
			<button class="ghost" onclick={() => (pinPrompt = { mode: 'change', reason: '' })}>
				Change PIN
			</button>
		{:else}
			<p class="warn">
				No PIN yet. Glance, Live and Console stay closed until you set one.
			</p>
			<button class="go" onclick={() => (pinPrompt = { mode: 'set', reason: '' })}>Set a PIN</button>
		{/if}
	</section>

	<section>
		<h2>Controllers</h2>
		{#each controllers as c (c.id)}
			<div class="row">
				<div class="who">
					<strong>{c.name}</strong>
					<span class="muted small">{c.os}{c.self ? ' · this device' : ''}</span>
				</div>
				{#if !c.self}<button class="ghost" onclick={() => remove('controller', c)}>Revoke</button>{/if}
			</div>
		{/each}
	</section>

	<section>
		<h2>Computers</h2>
		{#each nodes as n (n.id)}
			<div class="row">
				<div class="who">
					<strong>{n.name}</strong>
					<span class="muted small">{n.os} · {n.addr}</span>
				</div>
				<button class="ghost" onclick={() => remove('node', n)}>Remove</button>
			</div>
		{:else}
			<p class="muted">None yet.</p>
		{/each}
	</section>

	<section>
		<h2>Add a computer</h2>
		<p class="muted">
			On the Windows PC (with Tailscale signed in to this tailnet), open PowerShell and run:
		</p>
		<pre>irm {hubUrl}/dl/install-node.ps1 -OutFile $env:TEMP\install-node.ps1
powershell -ExecutionPolicy Bypass -File $env:TEMP\install-node.ps1 -Hub {hubUrl}</pre>
		<p class="muted small">
			It asks for administrator rights once, installs the service, and then appears above under
			"Waiting for approval".
		</p>
	</section>

	<section>
		<h2>Android app</h2>
		{#if apk === null}
			<p class="muted">No APK published yet (<code>.\deploy\build.ps1 -Android -Deploy</code>).</p>
		{:else}
			<a class="go link-button" href="/dl/remoter.apk" download>
				Download APK{apk ? ` (${(apk / 1048576).toFixed(1)} MB)` : ''}
			</a>
			<p class="muted small">
				Keeps the job list live in the background and can unlock the PIN with your
				fingerprint. Install it over Wi-Fi or mobile data: about 3 MB.
			</p>
		{/if}
	</section>

	<section>
		<h2>Activity</h2>
		{#if audit === null}
			<button class="ghost" onclick={loadAudit}>Show activity</button>
		{:else}
			<ul class="audit">
				{#each audit as e, i (i)}
					<li>
						<span class="muted">{when(e.time)}</span>
						<span>{describe(e)}</span>
					</li>
				{:else}
					<li class="muted">Nothing recorded yet.</li>
				{/each}
			</ul>
		{/if}
	</section>
</main>

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
	header {
		position: sticky;
		top: 0;
		z-index: 5;
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.75rem 1rem;
		background: var(--surface);
		border-bottom: 1px solid var(--line);
	}
	.back {
		color: var(--accent);
		text-decoration: none;
	}
	.spacer {
		flex: 1;
	}
	main {
		max-width: 640px;
		margin: 0 auto;
		padding: 0.5rem 1rem 3rem;
	}
	section {
		margin-top: 1.25rem;
	}
	h2 {
		font-size: 0.8rem;
		text-transform: uppercase;
		letter-spacing: 0.08em;
		color: var(--muted);
		margin: 0 0 0.6rem;
	}
	.row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		min-height: 56px;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--line);
	}
	.who {
		display: flex;
		flex-direction: column;
		flex: 1;
		min-width: 0;
	}
	button,
	.link-button {
		min-height: 40px;
		padding: 0 1rem;
		border-radius: 8px;
		font: inherit;
	}
	.go {
		background: var(--accent);
		color: #06121d;
		border: 0;
		font-weight: 600;
	}
	.ghost {
		background: transparent;
		border: 1px solid var(--line);
	}
	.link-button {
		display: inline-flex;
		align-items: center;
		text-decoration: none;
	}
	button.link {
		min-height: 0;
		padding: 0;
		background: none;
		border: 0;
		color: var(--muted);
		text-decoration: underline;
		font-size: 0.8rem;
	}
	pre {
		margin: 0.5rem 0;
		padding: 0.6rem;
		overflow-x: auto;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 6px;
		font-size: 0.75rem;
		user-select: all;
	}
	.audit {
		list-style: none;
		margin: 0;
		padding: 0;
		font-size: 0.85rem;
	}
	.audit li {
		display: flex;
		gap: 0.75rem;
		padding: 0.4rem 0;
		border-bottom: 1px solid var(--line);
	}
	.audit li span:first-child {
		flex: none;
		width: 7.5rem;
	}
	.muted {
		color: var(--muted);
	}
	.small {
		font-size: 0.78rem;
	}
	.err {
		color: var(--err);
	}
	.warn {
		color: var(--warn);
	}
	code {
		background: var(--surface-2);
		padding: 0.1rem 0.3rem;
		border-radius: 4px;
		font-size: 0.85em;
	}
</style>
