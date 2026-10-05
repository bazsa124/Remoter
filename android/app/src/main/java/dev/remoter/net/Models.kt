package dev.remoter.net

import kotlinx.serialization.Serializable

@Serializable
data class ActionView(
	val id: String,
	val label: String,
	val icon: String? = null,
	val confirm: Boolean = false,
	val available: Boolean = true,
	val reason: String? = null,
)

@Serializable
data class Job(
	val id: String,
	val actionId: String,
	val label: String,
	val state: String,
	val exit: Int? = null,
	val error: String? = null,
	val started: String,
	val ended: String? = null,
) {
	val terminal: Boolean
		get() = state == "done" || state == "failed" || state == "cancelled" || state == "timeout"
}

@Serializable
data class Tiers(
	val actions: Boolean = false,
	val glance: Boolean = false,
	val console: Boolean = false,
	val live: Boolean = false,
	val glanceReason: String? = null,
	val actionsReason: String? = null,
	val consoleReason: String? = null,
)

@Serializable
data class Disarm(val at: String, val reason: String)

@Serializable
data class ArmState(
	val supported: Boolean = false,
	val armed: Boolean = false,
	val since: String? = null,
	val until: String? = null,
	val lastDisarm: Disarm? = null,
	val error: String? = null,
)

@Serializable
data class Power(val onAC: Boolean = false, val hasBattery: Boolean = false, val percent: Int = -1)

/** A device's own report, relayed by the hub. */
@Serializable
data class Health(
	val version: String = "",
	val name: String = "",
	val mode: String = "",
	val os: String = "",
	val user: String? = null,
	val tiers: Tiers = Tiers(),
	val arm: ArmState? = null,
	val power: Power? = null,
)

@Serializable
data class Session(
	val controller: String,
	val controllerId: String = "",
	val tier: String,
	val since: String = "",
)

/** A controllable machine, as the hub lists it. */
@Serializable
data class Device(
	val id: String,
	val name: String,
	val os: String = "",
	val online: Boolean = false,
	val lastSeen: String? = null,
	val version: String = "",
	val health: Health? = null,
	val sessions: List<Session> = emptyList(),
)

@Serializable
data class PinStatus(
	val set: Boolean = false,
	val granted: Boolean = false,
	val held: Boolean = false,
	val seconds: Int = 0,
)

@Serializable
data class MeDevice(val id: String, val name: String, val os: String = "")

@Serializable
data class HubInfo(val name: String = "", val version: String = "")

/** Who the hub says this phone is, and whether it is let in. */
@Serializable
data class Me(
	val device: MeDevice,
	val state: String,
	val pin: PinStatus = PinStatus(),
	val hub: HubInfo = HubInfo(),
) {
	val approved: Boolean get() = state == "approved"
}

@Serializable
data class JobDetail(val job: Job, val tail: List<String> = emptyList(), val dropped: Int = 0)

/** Events pushed over the Tier 1 socket. */
@Serializable
data class AgentEvent(
	val type: String,
	val job: Job? = null,
	val jobId: String? = null,
	val line: String? = null,
)

/** Raised when the device demands the second step of a confirm action. */
class ConfirmRequired(val nonce: String, val expiresIn: Int) : Exception("confirmation required")

/** Raised while this phone waits for the owner's approval on the hub. */
class PendingApproval(message: String) : Exception(message)

/** Raised when the hub wants the PIN, or wants one set first ([unset]). */
class PinNeeded(message: String, val unset: Boolean) : Exception(message)

/** Raised when the hub rejects a PIN; [locked] after too many wrong attempts. */
class PinRejected(message: String, val locked: Boolean) : Exception(message)
