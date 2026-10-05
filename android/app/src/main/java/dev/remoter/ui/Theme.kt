package dev.remoter.ui

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

// Same palette as the web client, so the two do not feel like different products.
val Bg = Color(0xFF0B0D10)
val Surface = Color(0xFF14181D)
val Surface2 = Color(0xFF1C222A)
val Line = Color(0xFF2A323C)
val TextMain = Color(0xFFE6EAF0)
val Muted = Color(0xFF8B98A8)
val Accent = Color(0xFF5EB0EF)
val Ok = Color(0xFF4ADE80)
val Warn = Color(0xFFFBBF24)
val Err = Color(0xFFF87171)

private val scheme = darkColorScheme(
	primary = Accent,
	onPrimary = Color(0xFF06121D),
	background = Bg,
	onBackground = TextMain,
	surface = Surface,
	onSurface = TextMain,
	surfaceVariant = Surface2,
	onSurfaceVariant = Muted,
	outline = Line,
	error = Err,
)

@Composable
fun RemoterTheme(content: @Composable () -> Unit) =
	MaterialTheme(colorScheme = scheme, content = content)

fun stateColor(state: String): Color = when (state) {
	"done" -> Ok
	"running", "queued" -> Accent
	"cancelled" -> Muted
	else -> Err
}

fun formatBytes(n: Long): String =
	if (n < 1024) "$n B" else String.format("%.1f KB", n / 1024.0)
