<script lang="ts">
	// The PIN is the second lock on the screen and shell tiers, and on approving
	// devices. Tailscale already proved which device this is; the PIN proves the
	// person holding it is the owner.
	import { agent } from '$lib/agent.svelte';

	let {
		mode,
		reason = '',
		ondone,
		oncancel
	}: {
		mode: 'enter' | 'set' | 'change';
		reason?: string;
		ondone: () => void;
		oncancel: () => void;
	} = $props();

	let pin = $state('');
	let confirm = $state('');
	let current = $state('');
	let error = $state('');
	let busy = $state(false);

	const valid = (p: string) => /^\d{4,12}$/.test(p);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		error = '';
		if (mode !== 'enter') {
			if (!valid(pin)) return void (error = 'Use 4 to 12 digits.');
			if (pin !== confirm) return void (error = 'The two PINs do not match.');
		}
		busy = true;
		try {
			if (mode === 'enter') await agent.enterPin(pin);
			else await agent.setPin(pin, mode === 'change' ? current : undefined);
			ondone();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			pin = '';
		} finally {
			busy = false;
		}
	}
</script>

<div class="scrim">
	<form class="dialog" onsubmit={submit}>
		<h3>
			{mode === 'enter' ? 'Enter PIN' : mode === 'set' ? 'Set a PIN' : 'Change PIN'}
		</h3>
		<p class="muted">
			{#if reason}{reason}{:else if mode === 'enter'}
				Needed for Glance, Live, Console and approvals. Stays unlocked while you use them, and
				for 5 minutes after.
			{:else}
				Protects the screen and shell tiers on every device, and approving new ones. 4-12
				digits.
			{/if}
		</p>
		{#if mode === 'change'}
			<input
				type="password"
				inputmode="numeric"
				autocomplete="off"
				placeholder="Current PIN"
				bind:value={current}
			/>
		{/if}
		<!-- svelte-ignore a11y_autofocus -->
		<input
			type="password"
			inputmode="numeric"
			autocomplete="off"
			placeholder={mode === 'enter' ? 'PIN' : 'New PIN'}
			bind:value={pin}
			autofocus
		/>
		{#if mode !== 'enter'}
			<input
				type="password"
				inputmode="numeric"
				autocomplete="off"
				placeholder="Repeat new PIN"
				bind:value={confirm}
			/>
		{/if}
		{#if error}<p class="err">{error}</p>{/if}
		<div class="row">
			<button type="button" onclick={oncancel}>Cancel</button>
			<span class="spacer"></span>
			<button type="submit" class="primary" disabled={busy || !pin}>
				{busy ? '…' : mode === 'enter' ? 'Unlock' : 'Save'}
			</button>
		</div>
	</form>
</div>

<style>
	.scrim {
		position: fixed;
		inset: 0;
		z-index: 20;
		display: grid;
		place-items: center;
		padding: 1rem;
		background: rgba(0, 0, 0, 0.6);
	}
	.dialog {
		display: flex;
		flex-direction: column;
		gap: 0.6rem;
		width: 100%;
		max-width: 340px;
		padding: 1.25rem;
		background: var(--surface);
		border: 1px solid var(--line);
		border-radius: 12px;
	}
	h3 {
		margin: 0;
	}
	p {
		margin: 0;
		font-size: 0.85rem;
	}
	input {
		min-height: 48px;
		padding: 0 0.75rem;
		background: var(--bg);
		border: 1px solid var(--line);
		border-radius: 8px;
		color: var(--text);
		font: inherit;
		letter-spacing: 0.3em;
	}
	.row {
		display: flex;
		align-items: center;
		margin-top: 0.25rem;
	}
	.spacer {
		flex: 1;
	}
	button {
		min-height: 44px;
		padding: 0 1rem;
		background: transparent;
		border: 1px solid var(--line);
		border-radius: 8px;
	}
	button.primary {
		background: var(--accent);
		color: #06121d;
		border: 0;
		font-weight: 600;
	}
	button:disabled {
		opacity: 0.5;
	}
	.muted {
		color: var(--muted);
	}
	.err {
		color: var(--err);
	}
</style>
