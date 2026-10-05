package dev.remoter.ui

import android.annotation.SuppressLint
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import dev.remoter.net.HubClient
import dev.remoter.node.NodeService
import dev.remoter.node.NodeSettings
import dev.remoter.node.PhoneControl
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * Making this phone a target: the steps Android needs, each with its own
 * button, re-checked continuously so the list updates as the owner comes back
 * from each settings screen.
 */
@Composable
fun ThisPhoneScreen(client: HubClient, onBack: () -> Unit) {
	BackHandler(onBack = onBack)
	val context = LocalContext.current
	val node = remember { NodeSettings(context) }
	val scope = rememberCoroutineScope()

	var a11y by remember { mutableStateOf(false) }
	var battery by remember { mutableStateOf(false) }
	var tailnet by remember { mutableStateOf<String?>(null) }
	var enabled by remember { mutableStateOf(node.enabled) }
	var state by remember { mutableStateOf(node.enrolment) }
	var busy by remember { mutableStateOf(false) }
	var error by remember { mutableStateOf("") }

	LaunchedEffect(Unit) {
		while (true) {
			a11y = accessibilityEnabled(context)
			battery = context.getSystemService(PowerManager::class.java)
				.isIgnoringBatteryOptimizations(context.packageName)
			tailnet = NodeService.tailnetIp()
			delay(1_500)
		}
	}

	fun enable() {
		busy = true
		error = ""
		scope.launch {
			runCatching {
				val version = context.packageManager.getPackageInfo(context.packageName, 0).versionName ?: "?"
				client.enrollNode(node.token, NodeSettings.PORT, version)
			}.onSuccess { res ->
				node.hubAddr = res.hubAddr
				node.enrolment = res.state
				node.enabled = true
				state = res.state
				enabled = true
				NodeService.start(context)
			}.onFailure { error = it.message ?: "enrolment failed" }
			busy = false
		}
	}

	fun disable() {
		node.enabled = false
		enabled = false
		NodeService.stop(context)
	}

	Column(Modifier.fillMaxSize().background(Bg)) {
		Row(
			Modifier.fillMaxWidth().background(Surface).padding(4.dp),
			verticalAlignment = Alignment.CenterVertically,
		) {
			TextButton(onClick = onBack) { Text("← Back") }
			Text("Control this phone", color = TextMain, fontWeight = FontWeight.Bold)
		}
		Column(Modifier.padding(16.dp).verticalScroll(rememberScrollState())) {
			Text(
				"Lets your other devices see and use this phone through the hub - Glance and Live, " +
					"with taps, swipes, typing and Back/Home/Recents. It works unattended, with the " +
					"phone in a drawer, at a few frames a second. Every session shows a notification here.",
				color = Muted,
				fontSize = 13.sp,
			)

			Step(
				done = a11y,
				title = "1. Turn on the Accessibility service",
				body = "Settings → Accessibility → Downloaded apps → \"Remoter remote control\" → On. " +
					"Android may grey it out as a \"restricted setting\" because this app was not installed " +
					"from a store: then open App info first, tap ⋮ → \"Allow restricted settings\", and come back.",
			) {
				Row {
					OutlinedButton(onClick = { context.startActivity(Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS)) }) {
						Text("Accessibility")
					}
					Spacer(Modifier.width(8.dp))
					OutlinedButton(onClick = { openAppInfo(context) }) { Text("App info") }
				}
			}

			Step(
				done = battery,
				title = "2. Let it run in the background",
				body = "Without this, Android (and HyperOS even more so) stops the listener within minutes. " +
					"On Xiaomi phones also turn on Autostart in App info.",
			) {
				Row {
					OutlinedButton(onClick = { requestBatteryExemption(context) }) { Text("Allow") }
					Spacer(Modifier.width(8.dp))
					OutlinedButton(onClick = { openAppInfo(context) }) { Text("App info") }
				}
			}

			Step(
				done = tailnet != null,
				title = "3. Tailscale connected",
				body = tailnet?.let { "This phone is $it on the tailnet." }
					?: "Open Tailscale and connect. The hub reaches this phone only over the tailnet.",
			) {}

			Step(
				done = enabled && state == "approved",
				title = "4. Enrol with the hub",
				body = when {
					!enabled -> "Registers this phone with the hub. It then needs your approval on the Access page."
					state == "approved" -> "Approved. This phone appears in every controller's device list."
					else -> "Waiting for approval: open Access and approve this phone under \"wants to be controllable\"."
				},
			) {
				if (!enabled) {
					Button(enabled = !busy && tailnet != null, onClick = { enable() }) {
						Text(if (busy) "Enrolling…" else "Make this phone controllable")
					}
				} else {
					Row {
						OutlinedButton(enabled = !busy, onClick = { enable() }) { Text("Check again") }
						Spacer(Modifier.width(8.dp))
						OutlinedButton(onClick = { disable() }) { Text("Turn off", color = Err) }
					}
				}
			}
			if (error.isNotEmpty()) Text(error, color = Err, fontSize = 13.sp)

			Spacer(Modifier.height(12.dp))
			Text(
				"Limits: Android allows only a few screenshots a second this way, so Live is slow but " +
					"works with nobody at the phone. Secure screens (banking apps, the lock screen PIN) " +
					"show black. Typing replaces the focused field's text; it is not a keyboard.",
				color = Muted,
				fontSize = 12.sp,
			)
		}
	}
}

@Composable
private fun Step(done: Boolean, title: String, body: String, actions: @Composable () -> Unit) {
	Card(
		colors = CardDefaults.cardColors(containerColor = Surface),
		modifier = Modifier.fillMaxWidth().padding(top = 12.dp),
	) {
		Column(Modifier.padding(14.dp)) {
			Row(verticalAlignment = Alignment.CenterVertically) {
				Text(if (done) "✓" else "•", color = if (done) Ok else Warn, fontWeight = FontWeight.Bold)
				Spacer(Modifier.width(8.dp))
				Text(title, color = TextMain, fontWeight = FontWeight.SemiBold)
			}
			Spacer(Modifier.height(4.dp))
			Text(body, color = Muted, fontSize = 13.sp)
			if (!done) {
				Spacer(Modifier.height(8.dp))
				actions()
			}
		}
	}
}

/** Enabled in settings - even before the system has bound the service. */
private fun accessibilityEnabled(context: Context): Boolean {
	if (PhoneControl.instance != null) return true
	val enabled = Settings.Secure.getString(context.contentResolver, Settings.Secure.ENABLED_ACCESSIBILITY_SERVICES)
		.orEmpty()
	val me = ComponentName(context, PhoneControl::class.java)
	return enabled.split(':').any { ComponentName.unflattenFromString(it) == me }
}

private fun openAppInfo(context: Context) {
	context.startActivity(
		Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${context.packageName}"))
	)
}

@SuppressLint("BatteryLife") // sideloaded, never on the Play Store: this is the documented way
private fun requestBatteryExemption(context: Context) {
	val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${context.packageName}"))
	runCatching { context.startActivity(intent) }
		.onFailure { context.startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) }
}
