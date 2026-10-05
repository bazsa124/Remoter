// Reactive client for the hub and, through it, the selected device.
//
// Every request goes to the hub, which knows who we are from the tailnet -
// there is no token to paste, store or leak. Device requests are relayed under
// /d/{device}/, so a tier talks to whichever machine is selected without
// knowing where that machine is.
//
// The byte counter is not decoration: on a metered plan, knowing what a tier
// costs is the point of the whole tiered design. Every response advertises its
// size via X-Remoter-Bytes, and socket frames are counted as they arrive.

export type ActionView = {
	id: string;
	label: string;
	icon?: string;
	confirm: boolean;
	available: boolean;
	reason?: string;
};

export type JobState = 'queued' | 'running' | 'done' | 'failed' | 'cancelled' | 'timeout';

export type Job = {
	id: string;
	actionId: string;
	label: string;
	state: JobState;
	exit?: number;
	error?: string;
	started: string;
	ended?: string;
};

type Event =
	| { type: 'job'; job: Job }
	| { type: 'log'; jobId: string; line: string };

export type Monitor = { index: number; width: number; height: number; primary: boolean };

export type Tiers = {
	actions: boolean;
	glance: boolean;
	console: boolean;
	live: boolean;
	glanceReason?: string;
	actionsReason?: string;
	consoleReason?: string;
};

export type ArmState = {
	supported: boolean;
	armed: boolean;
	since?: string;
	until?: string;
	lastDisarm?: { at: string; reason: string };
	error?: string;
};

export type Health = {
	version: string;
	name: string;
	mode: string;
	os: string;
	user?: string;
	tiers: Tiers;
	arm?: ArmState;
	power?: { onAC: boolean; hasBattery: boolean; percent: number };
};

export type Session = { controller: string; controllerId: string; tier: string; since: string };

export type Device = {
	id: string;
	name: string;
	os: string;
	online: boolean;
	lastSeen?: string;
	version: string;
	health?: Health;
	sessions: Session[];
};

export type Me = {
	device: { id: string; name: string; os: string };
	state: 'approved' | 'pending';
	pin: { set: boolean; granted: boolean; held: boolean; seconds: number };
	hub: { name: string; version: string };
};

export type AccessEntry = {
	id: string;
	name: string;
	os: string;
	state: 'approved' | 'pending';
	since: string;
	self?: boolean;
	addr?: string;
};

export type Access = { controllers: AccessEntry[]; nodes: AccessEntry[] };

export type AuditEntry = {
	time: string;
	event: string;
	controller?: string;
	device?: string;
	tier?: string;
	detail?: string;
};

export type Glance = { url: string; bytes: number; dimensions: string; at: Date };

const DEVICE_KEY = 'remoter.device';

/** Thrown when the agent demands the second step of a confirm action. */
export class ConfirmNeeded extends Error {
	constructor(
		readonly nonce: string,
		readonly expiresIn: number
	) {
		super('confirmation required');
	}
}

/** Thrown when the hub wants the PIN (code pin_required) or one set (pin_unset). */
export class PinNeeded extends Error {
	constructor(
		message: string,
		readonly unset: boolean
	) {
		super(message);
	}
}

/** Thrown while this device waits for the owner's approval. */
export class Pending extends Error {}

function readDevice(): string {
	try {
		return localStorage.getItem(DEVICE_KEY) ?? '';
	} catch {
		return '';
	}
}

export class Agent {
	me = $state<Me | null>(null);
	devices = $state<Device[]>([]);
	device = $state(readDevice());
	connected = $state(false);
	error = $state('');

	actions = $state<ActionView[]>([]);
	jobs = $state<Job[]>([]);
	logs = $state<Record<string, string[]>>({});
	monitors = $state<Monitor[]>([]);

	/** Bytes received this session, all tiers. */
	bytes = $state(0);

	#socket: WebSocket | null = null;
	#retry = 0;
	#stopped = false;
	#generation = 0; // bumps on device switch; stale responses are dropped

	get current(): Device | null {
		return this.devices.find((d) => d.id === this.device) ?? null;
	}

	get health(): Health | null {
		return this.current?.health ?? null;
	}

	get approved() {
		return this.me?.state === 'approved';
	}

	/** Path prefix for the selected device's API. */
	get base() {
		return `/d/${encodeURIComponent(this.device)}`;
	}

	async #fetch(path: string, init: RequestInit = {}): Promise<Response> {
		const res = await fetch(path, { ...init, credentials: 'same-origin' });
		const size = res.headers.get('X-Remoter-Bytes');
		if (size) this.bytes += Number(size);
		return res;
	}

	/** Turns the hub's error codes into exceptions callers can branch on. */
	async #check(res: Response): Promise<Response> {
		if (res.ok) return res;
		const body = await res.json().catch(() => ({ error: res.statusText }));
		const msg = body.error ?? `HTTP ${res.status}`;
		if (body.code === 'pending') throw new Pending(msg);
		if (body.code === 'pin_required') throw new PinNeeded(msg, false);
		if (body.code === 'pin_unset') throw new PinNeeded(msg, true);
		throw new Error(msg);
	}

	async #json<T>(path: string, init?: RequestInit): Promise<T> {
		const res = await this.#check(await this.#fetch(path, init));
		if (res.status === 204) return undefined as T;
		return res.json() as Promise<T>;
	}

	#send<T>(method: string, path: string, body?: unknown): Promise<T> {
		return this.#json<T>(path, {
			method,
			headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
			body: body === undefined ? undefined : JSON.stringify(body)
		});
	}

	// --- hub -----------------------------------------------------------------

	async refreshMe(): Promise<Me> {
		this.me = await this.#json<Me>('/hub/me');
		return this.me;
	}

	async refreshDevices(): Promise<Device[]> {
		const list = await this.#json<Device[]>('/hub/devices');
		this.devices = list;
		// Keep the selection if it still exists; otherwise prefer an online device.
		if (!list.some((d) => d.id === this.device)) {
			const pick = list.find((d) => d.online) ?? list[0];
			if (pick) this.#setDevice(pick.id);
		}
		return list;
	}

	#setDevice(id: string) {
		this.device = id;
		try {
			localStorage.setItem(DEVICE_KEY, id);
		} catch {
			/* private mode: selection just is not remembered */
		}
	}

	access = () => this.#json<Access>('/hub/access');
	approve = (kind: 'controller' | 'node', id: string) =>
		this.#send<Access>('POST', `/hub/access/${kind}/${encodeURIComponent(id)}/approve`);
	removeAccess = (kind: 'controller' | 'node', id: string) =>
		this.#send<Access>('DELETE', `/hub/access/${kind}/${encodeURIComponent(id)}`);
	audit = (limit = 100) => this.#json<AuditEntry[]>(`/hub/audit?limit=${limit}`);

	async enterPin(pin: string) {
		await this.#send('POST', '/hub/pin', { pin });
		await this.refreshMe();
	}

	async setPin(pin: string, current?: string) {
		await this.#send('PUT', '/hub/pin', { pin, current });
		await this.refreshMe();
	}

	async lock() {
		await this.#send('DELETE', '/hub/pin/grant');
		await this.refreshMe();
	}

	/**
	 * Resolves when the guarded tiers are unlocked; throws PinNeeded otherwise.
	 * Asked before opening Live or Console, because a refused WebSocket
	 * handshake reaches the page with no reason attached.
	 */
	async requirePin() {
		const me = await this.refreshMe();
		if (!me.pin.set) throw new PinNeeded('Set a PIN to protect the screen and shell tiers', true);
		if (!me.pin.granted) throw new PinNeeded('Enter the PIN', false);
	}

	// --- the selected device ---------------------------------------------------

	/** Starts everything: identity, device list, then the selected device. */
	async start() {
		this.error = '';
		try {
			await this.refreshMe();
			if (!this.approved) return;
			await this.refreshDevices();
			await this.select(this.device);
		} catch (e) {
			if (!(e instanceof Pending)) this.error = e instanceof Error ? e.message : String(e);
		}
	}

	/** Switches to a device: the socket, actions and jobs all follow it. */
	async select(id: string) {
		if (!id) return;
		this.disconnect();
		this.#setDevice(id);
		const gen = ++this.#generation;
		this.actions = [];
		this.jobs = [];
		this.logs = {};
		this.monitors = [];
		this.error = '';

		const dev = this.current;
		if (!dev?.online) return;
		try {
			const tiers = dev.health?.tiers;
			const [actions, jobs, monitors] = await Promise.all([
				tiers?.actions ? this.#json<ActionView[]>(`${this.base}/api/actions`) : [],
				tiers?.actions ? this.#json<Job[]>(`${this.base}/api/jobs`) : [],
				tiers?.glance ? this.#json<Monitor[]>(`${this.base}/api/monitors`).catch(() => []) : []
			]);
			if (gen !== this.#generation) return;
			this.actions = actions;
			this.jobs = jobs;
			this.monitors = monitors;
			if (tiers?.actions) this.connect();
		} catch (e) {
			if (gen === this.#generation) this.error = e instanceof Error ? e.message : String(e);
		}
	}

	/**
	 * Fetches one Tier 1.5 frame.
	 *
	 * Through fetch rather than a plain <img src>, so every frame is counted -
	 * the entire reason this tier exists as a separate rung.
	 */
	async glance(monitor: number, quality: number, scale: number): Promise<Glance> {
		const params = new URLSearchParams({
			monitor: String(monitor),
			quality: String(quality),
			scale: String(scale)
		});
		const res = await this.#check(await this.#fetch(`${this.base}/api/screenshot?${params}`));
		const blob = await res.blob();
		this.bytes += blob.size;
		return {
			url: URL.createObjectURL(blob),
			bytes: blob.size,
			dimensions: res.headers.get('X-Remoter-Dimensions') ?? '',
			at: new Date()
		};
	}

	/**
	 * Runs an action. Throws ConfirmNeeded when the device wants the second
	 * step; call again with the nonce to go through with it.
	 */
	async run(id: string, nonce?: string): Promise<string> {
		const res = await this.#fetch(`${this.base}/api/actions/${encodeURIComponent(id)}/run`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(nonce ? { nonce } : {})
		});
		const body = await res.clone().json().catch(() => ({}));
		if (res.status === 409 && body.nonce) {
			throw new ConfirmNeeded(body.nonce, body.expiresIn ?? 60);
		}
		await this.#check(res);
		return body.jobId as string;
	}

	async cancel(jobId: string) {
		await this.#send('DELETE', `${this.base}/api/jobs/${jobId}`);
	}

	/** Fetches the retained output for one job. */
	async tail(jobId: string): Promise<string[]> {
		const res = await this.#json<{ tail: string[] }>(`${this.base}/api/jobs/${jobId}`);
		this.logs[jobId] = res.tail;
		return res.tail;
	}

	/** Keeps the device awake for `hours` (0 = until disarmed). */
	async arm(hours: number) {
		await this.#send('POST', `${this.base}/api/arm`, { hours });
		await this.refreshDevices();
	}

	async disarm() {
		await this.#send('POST', `${this.base}/api/disarm`, {});
		await this.refreshDevices();
	}

	/** WebSocket URL for a device tier. */
	socketUrl(path: string) {
		const proto = location.protocol === 'https:' ? 'wss' : 'ws';
		return `${proto}://${location.host}${this.base}${path}`;
	}

	/**
	 * Opens the Tier 1 socket and keeps it open.
	 *
	 * The spec's invariant: this stays connected in every tier. Reconnection uses
	 * exponential backoff with jitter, because a phone on LTE will drop it often
	 * and a tight retry loop is how you burn a battery.
	 */
	connect() {
		if (this.#socket || !this.device) return;
		this.#stopped = false;

		const gen = this.#generation;
		const socket = new WebSocket(this.socketUrl('/api/stream'));
		this.#socket = socket;

		socket.onopen = () => {
			this.connected = true;
			this.#retry = 0;
		};

		socket.onmessage = (ev) => {
			this.bytes += typeof ev.data === 'string' ? ev.data.length : 0;
			if (gen === this.#generation) this.#apply(JSON.parse(ev.data) as Event);
		};

		socket.onclose = () => {
			if (this.#socket === socket) {
				this.connected = false;
				this.#socket = null;
			}
			if (this.#stopped || gen !== this.#generation) return;

			const backoff = Math.min(30_000, 500 * 2 ** this.#retry++);
			const jitter = Math.random() * 0.3 * backoff;
			setTimeout(() => {
				if (!this.#stopped && gen === this.#generation) this.connect();
			}, backoff + jitter);
		};

		socket.onerror = () => socket.close();
	}

	disconnect() {
		this.#stopped = true;
		this.#socket?.close();
		this.#socket = null;
		this.connected = false;
	}

	#apply(ev: Event) {
		if (ev.type === 'job') {
			const i = this.jobs.findIndex((j) => j.id === ev.job.id);
			if (i === -1) this.jobs = [ev.job, ...this.jobs];
			else this.jobs[i] = ev.job;
			return;
		}
		// Read the array back out of the state proxy before mutating it.
		// `(this.logs[id] ??= []).push(...)` looks equivalent but is not: `??=`
		// evaluates to the raw array it assigned, so pushing to that bypasses the
		// proxy and the UI never learns the line arrived.
		if (!this.logs[ev.jobId]) this.logs[ev.jobId] = [];
		this.logs[ev.jobId].push(ev.line);
	}
}

export const agent = new Agent();
