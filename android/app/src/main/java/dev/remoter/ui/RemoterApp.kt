package dev.remoter.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.fragment.app.FragmentActivity
import dev.remoter.net.*
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.time.LocalDate
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter

private enum class Tier(val label: String, val cost: String) {
	Actions("1 · Actions", "~KB"),
	Glance("1.5 · Glance", "~50 KB"),
	Console("2 · Console", "KB/s"),
	Live("3 · Live", "~64 kbps"),
}

/** Where the app stands with the hub. */
private sealed interface Phase {
	data object Loading : Phase
	data class Unreachable(val message: String) : Phase
	data class Pending(val me: Me) : Phase
	data object Ready : Phase
}

/** A PIN prompt, and what to do once it is satisfied. */
private class PinRequest(val unset: Boolean, val then: () -> Unit)

@Composable
fun RemoterApp(
	settings: HubSettings,
	client: HubClient,
	vault: PinVault,
	activity: FragmentActivity,
	onDevice: () -> Unit,
	onStop: () -> Unit,
	onFullscreen: (Boolean) -> Unit = {},
) {
	// Bumped to reconnect from scratch, e.g. after the hub address changed.
	var attempt by remember { mutableIntStateOf(0) }
	var phase by remember { mutableStateOf<Phase>(Phase.Loading) }
	var editingHub by remember { mutableStateOf(false) }

	// There is nothing to pair: the hub recognises this phone by its tailnet
	// identity. A new phone simply waits here until the owner approves it.
	LaunchedEffect(attempt) {
		phase = Phase.Loading
		while (true) {
			val next = runCatching { client.me() }.fold(
				onSuccess = { if (it.approved) Phase.Ready else Phase.Pending(it) },
				onFailure = { Phase.Unreachable(it.message ?: "cannot reach the hub") },
			)
			phase = next
			if (next == Phase.Ready) break
			delay(if (next is Phase.Pending) 5_000 else 10_000)
		}
	}

	when (val p = phase) {
		Phase.Loading -> CenterMessage("Connecting to the hub…")
		is Phase.Unreachable -> Unreachable(
			hub = settings.hubUrl,
			message = p.message,
			onRetry = { attempt++ },
			onChangeHub = { editingHub = true },
		)
		is Phase.Pending -> PendingApprovalScreen(p.me)
		Phase.Ready -> MainScreen(
			settings, client, vault, activity, onDevice, onStop, onFullscreen,
			onChangeHub = { editingHub = true },
		)
	}

	if (editingHub) {
		HubDialog(
			current = settings.hubUrl,
			onSave = {
				settings.hubUrl = it
				settings.activeDevice = ""
				editingHub = false
				AgentState.reset()
				onStop()
				attempt++
			},
			onDismiss = { editingHub = false },
		)
	}
}

@Composable
private fun MainScreen(
	settings: HubSettings,
	client: HubClient,
	vault: PinVault,
	activity: FragmentActivity,
	onDevice: () -> Unit,
	onStop: () -> Unit,
	onFullscreen: (Boolean) -> Unit,
	onChangeHub: () -> Unit,
) {
	var devices by remember { mutableStateOf<List<Device>>(emptyList()) }
	var loaded by remember { mutableStateOf(false) }
	var activeId by remember { mutableStateOf(settings.activeDevice) }
	var tier by remember { mutableStateOf(Tier.Actions) }
	var fullscreen by remember { mutableStateOf(false) }
	var picking by remember { mutableStateOf(false) }
	var showAccess by remember { mutableStateOf(false) }
	var showThisPhone by remember { mutableStateOf(false) }
	var menu by remember { mutableStateOf(false) }
	var pinPrompt by remember { mutableStateOf<PinRequest?>(null) }
	var error by remember { mutableStateOf("") }
	val scope = rememberCoroutineScope()

	val connected by AgentState.connected.collectAsState()
	val bytes by AgentState.bytes.collectAsState()

	// Switching devices rebuilds every tier and moves the background socket: the
	// previous machine's jobs must never show while the next one loads.
	fun select(id: String) {
		if (id == activeId) return
		settings.activeDevice = id
		activeId = id
		tier = Tier.Actions
		AgentState.reset()
		onStop()
		onDevice()
	}

	suspend fun refresh() {
		runCatching { client.devices() }
			.onSuccess { list ->
				devices = list
				loaded = true
				error = ""
				if (list.none { it.id == activeId }) {
					(list.firstOrNull { it.online } ?: list.firstOrNull())?.let { select(it.id) }
				}
			}
			.onFailure { error = it.message ?: "cannot reach the hub" }
	}

	val device = devices.firstOrNull { it.id == activeId }

	LaunchedEffect(Unit) {
		if (activeId.isNotEmpty()) onDevice()
		while (true) {
			refresh()
			// Watch closely while the selected device is away - it is most likely
			// rebooting, and the moment it is back is the moment that matters.
			val away = devices.firstOrNull { it.id == activeId }?.online == false
			delay(if (away) 3_000 else 15_000)
		}
	}

	val tiers = if (device?.online == true) device.health?.tiers else null

	// The screen and shell tiers sit behind the PIN. Asked before opening
	// them, because a refused WebSocket handshake reaches the page with no
	// reason attached.
	fun openTier(t: Tier) {
		if (t == Tier.Actions) {
			tier = t
			return
		}
		scope.launch {
			runCatching { client.requirePin() }
				.onSuccess { tier = t }
				.onFailure { e ->
					if (e is PinNeeded) pinPrompt = PinRequest(e.unset) { tier = t }
					else error = e.message ?: "cannot reach the hub"
				}
		}
	}

	if (showThisPhone) {
		ThisPhoneScreen(client) {
			showThisPhone = false
			scope.launch { refresh() }
		}
		return
	}

	if (showAccess) {
		AccessScreen(client) {
			showAccess = false
			scope.launch { refresh() }
		}
		return
	}

	val goFullscreen: (Boolean) -> Unit = { on ->
		fullscreen = on
		onFullscreen(on)
	}

	Scaffold(
		containerColor = Bg,
		topBar = {
			if (!fullscreen) Row(
				Modifier.fillMaxWidth().background(Surface).padding(start = 12.dp, top = 4.dp, bottom = 4.dp),
				verticalAlignment = Alignment.CenterVertically,
			) {
				Box(Modifier.size(9.dp).clip(CircleShape).background(if (connected) Ok else Err))
				Spacer(Modifier.width(8.dp))
				Row(
					verticalAlignment = Alignment.CenterVertically,
					modifier = Modifier.clickable { picking = true }.padding(vertical = 8.dp),
				) {
					Text(
						device?.name ?: "Remoter",
						fontWeight = FontWeight.Bold,
						color = TextMain,
						maxLines = 1,
						overflow = TextOverflow.Ellipsis,
						modifier = Modifier.widthIn(max = 180.dp),
					)
					Text(" ▾", color = Muted, fontSize = 12.sp)
				}
				Spacer(Modifier.weight(1f))
				Text(formatBytes(bytes), color = Muted, fontSize = 12.sp)
				Box {
					IconButton(onClick = { menu = true }) {
						Icon(Icons.Default.MoreVert, contentDescription = "Menu", tint = Muted)
					}
					DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
						DropdownMenuItem(
							text = { Text("Access & approvals") },
							onClick = { menu = false; showAccess = true },
						)
						DropdownMenuItem(
							text = { Text("Control this phone") },
							onClick = { menu = false; showThisPhone = true },
						)
						DropdownMenuItem(
							text = { Text("Hub address") },
							onClick = { menu = false; onChangeHub() },
						)
						if (vault.stored) DropdownMenuItem(
							text = { Text("Forget saved PIN") },
							onClick = { menu = false; vault.clear() },
						)
					}
				}
			}
		},
		bottomBar = {
			// The mode switcher is the product, so it gets permanent screen space
			// and each rung is labelled with what it costs to use - except when a
			// tier has asked for the entire screen.
			if (!fullscreen) NavigationBar(containerColor = Surface) {
				Tier.entries.forEach { t ->
					val enabled = when (t) {
						Tier.Actions -> true
						Tier.Glance -> tiers?.glance == true
						Tier.Console -> tiers?.console == true
						Tier.Live -> tiers?.live == true
					}
					NavigationBarItem(
						selected = tier == t,
						enabled = enabled,
						onClick = { openTier(t) },
						icon = {},
						label = {
							Column(horizontalAlignment = Alignment.CenterHorizontally) {
								Text(t.label, fontSize = 12.sp, fontWeight = FontWeight.SemiBold)
								Text(t.cost, fontSize = 9.sp, color = Muted)
							}
						},
					)
				}
			}
		},
	) { padding ->
		val inset = if (fullscreen) 0.dp else 16.dp
		Column(Modifier.padding(padding).padding(horizontal = inset)) {
			if (error.isNotEmpty() && !fullscreen) {
				Text(error, color = Err, fontSize = 13.sp, modifier = Modifier.padding(vertical = 8.dp))
			}

			// Keyed on the device so a switch tears down the old tier completely
			// rather than leaving a WebView pointed at the previous machine.
			key(activeId) {
				when {
					!loaded -> CenterMessage("Loading devices…")
					device == null -> NoDevices { showAccess = true }
					tier == Tier.Actions ->
						ActionsTier(client, device) { scope.launch { refresh() } }
					!device.online -> Reconnecting(device)
					tier == Tier.Glance -> GlanceTier(client, tiers) {
						pinPrompt = PinRequest(unset = false) {}
					}
					tier == Tier.Live -> LiveTier(client, tiers, fullscreen, goFullscreen)
					tier == Tier.Console -> ConsoleTier(client, tiers)
				}
			}
		}
	}

	if (picking) {
		DevicePicker(
			devices = devices,
			activeId = activeId,
			onSelect = { picking = false; select(it) },
			onAccess = { picking = false; showAccess = true },
			onDismiss = { picking = false },
		)
	}

	pinPrompt?.let { req ->
		PinDialog(
			client = client,
			vault = vault,
			activity = activity,
			unset = req.unset,
			onDone = {
				pinPrompt = null
				req.then()
			},
			onCancel = { pinPrompt = null },
		)
	}
}

// --- connection states --------------------------------------------------------

@Composable
private fun CenterMessage(text: String) {
	Box(Modifier.fillMaxSize().padding(24.dp), contentAlignment = Alignment.Center) {
		Text(text, color = Muted)
	}
}

@Composable
private fun Unreachable(hub: String, message: String, onRetry: () -> Unit, onChangeHub: () -> Unit) {
	Column(
		Modifier.fillMaxSize().padding(24.dp).verticalScroll(rememberScrollState()),
		verticalArrangement = Arrangement.Center,
	) {
		Text("Remoter", fontSize = 28.sp, fontWeight = FontWeight.Bold, color = TextMain)
		Spacer(Modifier.height(8.dp))
		Text("Cannot reach the hub at", color = Muted, fontSize = 13.sp)
		Text(hub, color = TextMain, fontFamily = FontFamily.Monospace, fontSize = 13.sp)
		Spacer(Modifier.height(8.dp))
		Text(message, color = Err, fontSize = 13.sp)
		Spacer(Modifier.height(8.dp))
		Text(
			"The hub only answers devices on the tailnet. Is Tailscale connected on this phone?",
			color = Muted,
			fontSize = 13.sp,
		)
		Spacer(Modifier.height(20.dp))
		Button(onClick = onRetry, modifier = Modifier.fillMaxWidth().height(50.dp)) { Text("Retry") }
		TextButton(onClick = onChangeHub, modifier = Modifier.fillMaxWidth()) { Text("Change hub address") }
	}
}

@Composable
private fun PendingApprovalScreen(me: Me) {
	Column(
		Modifier.fillMaxSize().padding(24.dp).verticalScroll(rememberScrollState()),
		verticalArrangement = Arrangement.Center,
	) {
		Text("Remoter", fontSize = 28.sp, fontWeight = FontWeight.Bold, color = TextMain)
		Spacer(Modifier.height(12.dp))
		Text(
			"${me.device.name} is waiting for approval.",
			color = TextMain,
			fontWeight = FontWeight.SemiBold,
		)
		Spacer(Modifier.height(8.dp))
		Text(
			"Approve it from a device you already use (menu → Access), or on the hub:",
			color = Muted,
			fontSize = 13.sp,
		)
		Spacer(Modifier.height(8.dp))
		Text(
			"sudo remoter-hub approve ${me.device.name}",
			color = TextMain,
			fontFamily = FontFamily.Monospace,
			fontSize = 13.sp,
			modifier = Modifier.fillMaxWidth().background(Surface, RoundedCornerShape(6.dp)).padding(10.dp),
		)
		Spacer(Modifier.height(12.dp))
		Text("This screen continues on its own once you are approved.", color = Muted, fontSize = 12.sp)
	}
}

@Composable
private fun HubDialog(current: String, onSave: (String) -> Unit, onDismiss: () -> Unit) {
	var value by remember { mutableStateOf(current) }
	AlertDialog(
		onDismissRequest = onDismiss,
		containerColor = Surface,
		title = { Text("Hub address") },
		text = {
			Column {
				Text("The jump server every device goes through.", color = Muted, fontSize = 13.sp)
				Spacer(Modifier.height(8.dp))
				OutlinedTextField(
					value = value,
					onValueChange = { value = it },
					singleLine = true,
					keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
					modifier = Modifier.fillMaxWidth(),
				)
			}
		},
		confirmButton = {
			TextButton(enabled = value.isNotBlank(), onClick = { onSave(value) }) { Text("Save") }
		},
		dismissButton = {
			TextButton(onClick = { value = HubSettings.DEFAULT_HUB }) { Text("Default") }
		},
	)
}

// --- devices -----------------------------------------------------------------

@Composable
private fun DevicePicker(
	devices: List<Device>,
	activeId: String,
	onSelect: (String) -> Unit,
	onAccess: () -> Unit,
	onDismiss: () -> Unit,
) {
	AlertDialog(
		onDismissRequest = onDismiss,
		containerColor = Surface,
		title = { Text("Devices") },
		text = {
			Column {
				if (devices.isEmpty()) {
					Text("No devices approved yet.", color = Muted)
				}
				devices.forEach { d ->
					Row(
						Modifier.fillMaxWidth().clickable { onSelect(d.id) }.padding(vertical = 10.dp),
						verticalAlignment = Alignment.CenterVertically,
					) {
						Box(Modifier.size(8.dp).clip(CircleShape).background(if (d.online) Ok else Err))
						Spacer(Modifier.width(10.dp))
						Column(Modifier.weight(1f)) {
							Text(
								d.name,
								color = if (d.id == activeId) Accent else TextMain,
								fontWeight = FontWeight.SemiBold,
							)
							val status = buildList {
								add(d.os)
								add(if (d.online) "online" else "offline")
								if (d.health?.arm?.armed == true) add("armed")
								if (d.sessions.isNotEmpty()) add("in use")
							}.joinToString(" · ")
							Text(status, color = Muted, fontSize = 11.sp)
						}
					}
				}
			}
		},
		confirmButton = { TextButton(onClick = onAccess) { Text("Access & approvals") } },
		dismissButton = { TextButton(onClick = onDismiss) { Text("Close") } },
	)
}

@Composable
private fun NoDevices(onAccess: () -> Unit) {
	Column(Modifier.fillMaxSize().padding(vertical = 24.dp)) {
		Text("No devices yet", color = TextMain, fontWeight = FontWeight.SemiBold)
		Spacer(Modifier.height(8.dp))
		Text(
			"Install the node on a computer (Access → Add a computer), then approve it there.",
			color = Muted,
			fontSize = 13.sp,
		)
		Spacer(Modifier.height(12.dp))
		Button(onClick = onAccess) { Text("Open Access") }
	}
}

/** Status, who is connected, and arming - for the selected device. */
@Composable
private fun DeviceCard(client: HubClient, device: Device, onChanged: () -> Unit) {
	val h = device.health
	Card(
		colors = CardDefaults.cardColors(containerColor = Surface),
		modifier = Modifier.fillMaxWidth().padding(top = 12.dp),
	) {
		Column(Modifier.padding(14.dp)) {
			Row(verticalAlignment = Alignment.CenterVertically) {
				Text(device.name, color = TextMain, fontWeight = FontWeight.Bold)
				Spacer(Modifier.width(8.dp))
				Text(device.os, color = Muted, fontSize = 12.sp)
				Spacer(Modifier.weight(1f))
				if (device.online) Text("online", color = Ok, fontSize = 12.sp)
				else Text("offline · seen ${ago(device.lastSeen)}", color = Err, fontSize = 12.sp)
			}
			if (!device.online || h == null) return@Column

			val facts = buildList {
				if (device.os != "android") {
					add(h.user?.takeIf { it.isNotBlank() }?.let { "signed in: $it" } ?: "nobody signed in")
				}
				h.power?.takeIf { it.hasBattery }?.let {
					add("battery ${it.percent}%" + if (it.onAC) " · on AC" else "")
				}
			}
			Text(facts.joinToString("   "), color = Muted, fontSize = 12.sp, modifier = Modifier.padding(top = 4.dp))

			if (device.sessions.isNotEmpty()) {
				Text(
					"Connected: " + device.sessions.joinToString { "${it.controller} (${tierName(it.tier)})" },
					color = Warn,
					fontSize = 12.sp,
					modifier = Modifier.padding(top = 6.dp),
				)
			}

			h.arm?.takeIf { it.supported }?.let { ArmControls(client, it, onChanged) }
		}
	}
}

private val armChoices = listOf(
	0.0 to "until disarmed",
	2.0 to "for 2 hours",
	8.0 to "for 8 hours",
	24.0 to "for 1 day",
	72.0 to "for 3 days",
	168.0 to "for 1 week",
)

@Composable
private fun ArmControls(client: HubClient, arm: ArmState, onChanged: () -> Unit) {
	val scope = rememberCoroutineScope()
	var choice by remember { mutableStateOf(armChoices.first()) }
	var open by remember { mutableStateOf(false) }
	var busy by remember { mutableStateOf(false) }
	var error by remember { mutableStateOf("") }

	fun act(block: suspend () -> Unit) {
		busy = true
		error = ""
		scope.launch {
			runCatching { block() }.onFailure { error = it.message ?: "failed" }
			busy = false
			onChanged()
		}
	}

	Row(Modifier.padding(top = 10.dp), verticalAlignment = Alignment.CenterVertically) {
		if (arm.armed) {
			Text(
				"Armed" + (arm.until?.let { " until ${clock(it)}" } ?: ""),
				color = Warn,
				fontWeight = FontWeight.SemiBold,
				modifier = Modifier.weight(1f),
			)
			OutlinedButton(enabled = !busy, onClick = { act { client.disarm() } }) { Text("Disarm") }
		} else {
			Box(Modifier.weight(1f)) {
				TextButton(onClick = { open = true }) { Text(choice.second + " ▾", color = TextMain) }
				DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
					armChoices.forEach { c ->
						DropdownMenuItem(text = { Text(c.second) }, onClick = { choice = c; open = false })
					}
				}
			}
			OutlinedButton(enabled = !busy, onClick = { act { client.arm(choice.first) } }) { Text("Arm") }
		}
	}
	val note = when {
		arm.armed ->
			"Stays awake with the lid closed. Disarms itself on low battery, or when someone uses it after it was left alone for 10 minutes."
		arm.lastDisarm != null && arm.lastDisarm.reason != "manual" ->
			"Disarmed itself ${ago(arm.lastDisarm.at)}: ${arm.lastDisarm.reason}."
		else -> "Arm before leaving: a sleeping laptop cannot be woken over Wi-Fi."
	}
	Text(note, color = Muted, fontSize = 11.sp, modifier = Modifier.padding(top = 4.dp))
	val problem = error.ifEmpty { arm.error.orEmpty() }
	if (problem.isNotEmpty()) Text(problem, color = Err, fontSize = 12.sp)
}

// --- tiers ---------------------------------------------------------------------

@Composable
private fun ActionsTier(client: HubClient, device: Device, onChanged: () -> Unit) {
	val scope = rememberCoroutineScope()
	var confirm by remember { mutableStateOf<Pair<ActionView, String>?>(null) }
	var selected by remember { mutableStateOf<String?>(null) }
	var actions by remember { mutableStateOf<List<ActionView>>(emptyList()) }
	var error by remember { mutableStateOf("") }

	val jobs by AgentState.jobs.collectAsState()
	val logs by AgentState.logs.collectAsState()
	val tiers = device.health?.tiers
	val usable = device.online && tiers?.actions == true

	LaunchedEffect(usable) {
		if (!usable) return@LaunchedEffect
		runCatching {
			actions = client.actions()
			AgentState.setJobs(client.jobs())
		}.onFailure { error = it.message ?: "cannot reach ${device.name}" }
	}

	fun run(a: ActionView, nonce: String? = null) {
		scope.launch {
			runCatching { client.run(a.id, nonce) }
				.onSuccess { id -> selected = id; confirm = null }
				.onFailure { e ->
					if (e is ConfirmRequired) confirm = a to e.nonce
					else error = e.message ?: "failed"
				}
		}
	}

	LazyColumn(Modifier.fillMaxSize()) {
		item { DeviceCard(client, device, onChanged) }

		if (!device.online) {
			item {
				Text(
					"${device.name} is not answering. If it is asleep it cannot be woken remotely - arm it before leaving next time.",
					color = Muted,
					fontSize = 13.sp,
					modifier = Modifier.padding(vertical = 12.dp),
				)
			}
			return@LazyColumn
		}
		if (error.isNotEmpty()) item { Text(error, color = Err, fontSize = 13.sp) }

		item { SectionLabel("Actions") }
		if (!usable) {
			item { Text(tiers?.actionsReason ?: "Actions are unavailable.", color = Muted, fontSize = 13.sp) }
			return@LazyColumn
		}
		if (actions.isEmpty()) {
			item { Text("No actions defined on ${device.name} (actions.yaml).", color = Muted, fontSize = 13.sp) }
		}

		items(actions, key = { it.id }) { a ->
			Card(
				colors = CardDefaults.cardColors(containerColor = Surface),
				modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)
					.clickable(enabled = a.available) { run(a) },
			) {
				Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
					Column(Modifier.weight(1f)) {
						Text(
							a.label,
							color = if (a.available) TextMain else Muted,
							fontWeight = FontWeight.SemiBold,
						)
						if (!a.available && a.reason != null) {
							Text(a.reason, color = Muted, fontSize = 11.sp)
						}
					}
					if (a.confirm) Text("confirm", color = Warn, fontSize = 11.sp)
				}
			}
		}

		item { SectionLabel("Jobs") }

		items(jobs, key = { it.id }) { job ->
			Row(
				Modifier.fillMaxWidth()
					.clickable {
						selected = if (selected == job.id) null else job.id
						// Socket events are the fast path, not the only one: fetch the
						// retained tail for anything that finished while we were away.
						if (logs[job.id] == null) {
							scope.launch {
								runCatching { client.jobDetail(job.id) }
									.onSuccess { AgentState.setTail(job.id, it.tail) }
							}
						}
					}
					.padding(vertical = 12.dp),
				verticalAlignment = Alignment.CenterVertically,
			) {
				Box(Modifier.size(8.dp).clip(CircleShape).background(stateColor(job.state)))
				Spacer(Modifier.width(10.dp))
				Text(
					job.label,
					color = TextMain,
					modifier = Modifier.weight(1f),
					maxLines = 1,
					overflow = TextOverflow.Ellipsis,
				)
				Text(job.state, color = stateColor(job.state), fontSize = 12.sp)
			}

			if (selected == job.id) {
				val lines = logs[job.id].orEmpty()
				Column(
					Modifier.fillMaxWidth()
						.border(1.dp, Line, RoundedCornerShape(8.dp))
						.background(Bg, RoundedCornerShape(8.dp))
						.padding(10.dp),
				) {
					Text(
						if (lines.isEmpty()) {
							// A successful build is silent; that must not look like failure.
							if (job.terminal) "This action produced no output."
							else "Waiting for output..."
						} else {
							lines.joinToString("\n")
						},
						color = TextMain,
						fontFamily = FontFamily.Monospace,
						fontSize = 11.sp,
					)
					if (!job.terminal) {
						Spacer(Modifier.height(8.dp))
						TextButton(onClick = {
							scope.launch { runCatching { client.cancel(job.id) } }
						}) { Text("Cancel", color = Err) }
					}
				}
			}
		}
	}

	val pending = confirm
	if (pending != null) {
		AlertDialog(
			onDismissRequest = { confirm = null },
			title = { Text(pending.first.label) },
			text = { Text("This action asks for confirmation. Run it on ${device.name}?") },
			confirmButton = {
				TextButton(onClick = { run(pending.first, pending.second) }) {
					Text("Run", color = Err)
				}
			},
			dismissButton = { TextButton(onClick = { confirm = null }) { Text("Cancel") } },
			containerColor = Surface,
		)
	}
}

@Composable
private fun GlanceTier(client: HubClient, tiers: Tiers?, onLocked: () -> Unit) {
	var quality by remember { mutableIntStateOf(60) }
	var scale by remember { mutableFloatStateOf(0.5f) }
	var frame by remember { mutableStateOf<android.graphics.Bitmap?>(null) }
	var size by remember { mutableLongStateOf(0L) }
	var busy by remember { mutableStateOf(false) }
	var error by remember { mutableStateOf("") }
	val scope = rememberCoroutineScope()

	fun capture() {
		busy = true
		error = ""
		scope.launch {
			runCatching { client.glance(0, quality, scale.toDouble()) }
				.onSuccess { data ->
					size = data.size.toLong()
					frame = android.graphics.BitmapFactory.decodeByteArray(data, 0, data.size)
					AgentState.addBytes(data.size.toLong())
				}
				.onFailure { e ->
					if (e is PinNeeded) onLocked() else error = e.message ?: "capture failed"
				}
			busy = false
		}
	}

	Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
		SectionLabel("Glance")

		if (tiers?.glance != true) {
			Text(tiers?.glanceReason ?: "Cannot capture a screen.", color = Err, fontSize = 13.sp)
			return@Column
		}

		Text("Quality $quality", color = Muted, fontSize = 12.sp)
		Slider(
			value = quality.toFloat(),
			onValueChange = { quality = it.toInt() },
			valueRange = 20f..90f,
		)
		Text("Scale " + "%.2f".format(scale), color = Muted, fontSize = 12.sp)
		Slider(value = scale, onValueChange = { scale = it }, valueRange = 0.2f..1f)

		Button(onClick = { capture() }, enabled = !busy, modifier = Modifier.fillMaxWidth()) {
			Text(if (busy) "Capturing..." else if (frame == null) "Capture" else "Refresh")
		}

		if (error.isNotEmpty()) Text(error, color = Err, fontSize = 13.sp)

		val bitmap = frame
		if (bitmap != null) {
			Spacer(Modifier.height(12.dp))
			Image(
				bitmap.asImageBitmap(),
				contentDescription = "Screen",
				modifier = Modifier.fillMaxWidth(),
			)
			Text(
				"${bitmap.width}x${bitmap.height} - ${formatBytes(size)}",
				color = Muted,
				fontSize = 11.sp,
			)
		}
	}
}

@Composable
private fun ConsoleTier(client: HubClient, tiers: Tiers?) {
	Column(Modifier.fillMaxSize()) {
		SectionLabel("Console")
		if (tiers?.console != true) {
			Text(tiers?.consoleReason ?: "Console is unavailable.", color = Err, fontSize = 13.sp)
			return@Column
		}
		EmbeddedPage(client.tierPage("console"), modifier = Modifier.weight(1f))
	}
}

@Composable
private fun LiveTier(
	client: HubClient,
	tiers: Tiers?,
	fullscreen: Boolean,
	onFullscreen: (Boolean) -> Unit,
) {
	Column(Modifier.fillMaxSize()) {
		if (tiers?.live != true) {
			SectionLabel("Live")
			Text(tiers?.glanceReason ?: "Live is unavailable on this device.", color = Err, fontSize = 13.sp)
			return@Column
		}
		// The section label is chrome too, and chrome is what fullscreen removes.
		if (!fullscreen) SectionLabel("Live")
		EmbeddedPage(client.tierPage("live"), onFullscreen = onFullscreen, modifier = Modifier.weight(1f))
	}
}

/**
 * Shown in place of a screen or shell tier while its device is unreachable -
 * usually mid-reboot. The tier stays selected and comes back by itself.
 */
@Composable
private fun Reconnecting(device: Device) {
	Column(Modifier.fillMaxSize().padding(vertical = 24.dp)) {
		Text("${device.name} is offline", color = TextMain, fontWeight = FontWeight.SemiBold)
		Spacer(Modifier.height(8.dp))
		Text(
			"Last seen ${ago(device.lastSeen)}. If it is restarting, this reopens on its own " +
				"once it is back - usually about a minute after a reboot.",
			color = Muted,
			fontSize = 13.sp,
		)
		Spacer(Modifier.height(16.dp))
		LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
	}
}

/** The hub's Access page: approvals, revocation, PIN, activity, downloads. */
@Composable
private fun AccessScreen(client: HubClient, onBack: () -> Unit) {
	BackHandler(onBack = onBack)
	Column(Modifier.fillMaxSize().background(Bg)) {
		Row(
			Modifier.fillMaxWidth().background(Surface).padding(4.dp),
			verticalAlignment = Alignment.CenterVertically,
		) {
			TextButton(onClick = onBack) { Text("← Back") }
			Text("Access", color = TextMain, fontWeight = FontWeight.Bold)
		}
		EmbeddedPage(client.accessPage(), modifier = Modifier.weight(1f))
	}
}

// --- PIN ------------------------------------------------------------------------

/**
 * Asks for the PIN - or sets the first one - and optionally keeps it behind the
 * fingerprint. The hub verifies it either way; the vault only saves typing.
 */
@Composable
private fun PinDialog(
	client: HubClient,
	vault: PinVault,
	activity: FragmentActivity,
	unset: Boolean,
	onDone: () -> Unit,
	onCancel: () -> Unit,
) {
	var pin by remember { mutableStateOf("") }
	var repeat by remember { mutableStateOf("") }
	var keep by remember { mutableStateOf(vault.available && !vault.stored) }
	var error by remember { mutableStateOf("") }
	var busy by remember { mutableStateOf(false) }
	val scope = rememberCoroutineScope()
	val valid = pin.length in 4..12 && pin.all { it.isDigit() }

	fun submit(value: String, fromVault: Boolean) {
		busy = true
		error = ""
		scope.launch {
			runCatching { if (unset) client.setPin(value) else client.enterPin(value) }
				.onSuccess {
					busy = false
					if (!fromVault && keep && vault.available) {
						// Saving needs a fingerprint too; carry on whatever the outcome.
						vault.save(activity, value) { _, _ -> onDone() }
					} else {
						onDone()
					}
				}
				.onFailure { e ->
					busy = false
					if (fromVault && e is PinRejected && !e.locked) {
						vault.clear()
						error = "The saved PIN no longer matches - type it."
					} else {
						error = e.message ?: "failed"
					}
				}
		}
	}

	fun fingerprint() {
		vault.unlock(activity) { value, err ->
			when {
				value != null -> submit(value, fromVault = true)
				err != null -> error = err
			}
		}
	}

	LaunchedEffect(Unit) {
		if (!unset && vault.stored) fingerprint()
	}

	AlertDialog(
		onDismissRequest = onCancel,
		containerColor = Surface,
		title = { Text(if (unset) "Set a PIN" else "Enter PIN") },
		text = {
			Column {
				Text(
					if (unset) "Protects Glance, Live, Console and approvals on every device. 4-12 digits."
					else "Needed for Glance, Live, Console and approvals. Stays unlocked while you use them, and for 5 minutes after.",
					color = Muted,
					fontSize = 13.sp,
				)
				Spacer(Modifier.height(10.dp))
				OutlinedTextField(
					value = pin,
					onValueChange = { pin = it.filter(Char::isDigit).take(12) },
					label = { Text(if (unset) "New PIN" else "PIN") },
					singleLine = true,
					visualTransformation = PasswordVisualTransformation(),
					keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword),
					modifier = Modifier.fillMaxWidth(),
				)
				if (unset) {
					Spacer(Modifier.height(8.dp))
					OutlinedTextField(
						value = repeat,
						onValueChange = { repeat = it.filter(Char::isDigit).take(12) },
						label = { Text("Repeat PIN") },
						singleLine = true,
						visualTransformation = PasswordVisualTransformation(),
						keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword),
						modifier = Modifier.fillMaxWidth(),
					)
				}
				if (vault.available && !vault.stored) {
					Row(verticalAlignment = Alignment.CenterVertically) {
						Checkbox(checked = keep, onCheckedChange = { keep = it })
						Text("Unlock with fingerprint next time", color = TextMain, fontSize = 13.sp)
					}
				}
				if (!unset && vault.stored) {
					TextButton(onClick = { fingerprint() }) { Text("Use fingerprint") }
				}
				if (error.isNotEmpty()) Text(error, color = Err, fontSize = 13.sp)
			}
		},
		confirmButton = {
			TextButton(
				enabled = !busy && valid,
				onClick = {
					if (unset && pin != repeat) error = "The two PINs do not match."
					else submit(pin, fromVault = false)
				},
			) { Text(if (busy) "…" else if (unset) "Save" else "Unlock") }
		},
		dismissButton = { TextButton(onClick = onCancel) { Text("Cancel") } },
	)
}

// --- helpers ------------------------------------------------------------------------

@Composable
private fun SectionLabel(text: String) {
	Text(
		text.uppercase(),
		color = Muted,
		fontSize = 11.sp,
		fontWeight = FontWeight.SemiBold,
		modifier = Modifier.padding(top = 16.dp, bottom = 6.dp),
	)
}

private fun tierName(t: String) = when (t) {
	"live" -> "Live"
	"console" -> "Console"
	"glance" -> "Glance"
	else -> t
}

private fun parse(iso: String?): OffsetDateTime? =
	iso?.let { runCatching { OffsetDateTime.parse(it) }.getOrNull() }

/** "14:05" today, "Tue 14:05" otherwise. */
private fun clock(iso: String): String {
	val t = parse(iso)?.atZoneSameInstant(ZoneId.systemDefault()) ?: return iso
	val pattern = if (t.toLocalDate() == LocalDate.now()) "HH:mm" else "EEE HH:mm"
	return t.format(DateTimeFormatter.ofPattern(pattern))
}

private fun ago(iso: String?): String {
	val t = parse(iso) ?: return "never"
	val s = java.time.Duration.between(t.toInstant(), java.time.Instant.now()).seconds
	return when {
		s < 90 -> "just now"
		s < 5400 -> "${s / 60} min ago"
		s < 129600 -> "${s / 3600} h ago"
		else -> t.toLocalDate().toString()
	}
}
