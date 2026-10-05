package dev.remoter.net

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update

/**
 * Shared, process-wide view of the host.
 *
 * The foreground service owns the socket and writes here; the UI observes. That
 * split is the reason this app exists: a WebView's socket dies when the page is
 * backgrounded, so job state would stop arriving exactly when you have put the
 * phone in your pocket to wait for it.
 */
object AgentState {

	private val _connected = MutableStateFlow(false)
	val connected: StateFlow<Boolean> = _connected

	private val _jobs = MutableStateFlow<List<Job>>(emptyList())
	val jobs: StateFlow<List<Job>> = _jobs

	private val _logs = MutableStateFlow<Map<String, List<String>>>(emptyMap())
	val logs: StateFlow<Map<String, List<String>>> = _logs

	private val _bytes = MutableStateFlow(0L)
	val bytes: StateFlow<Long> = _bytes

	fun setConnected(value: Boolean) { _connected.value = value }

	fun setJobs(list: List<Job>) { _jobs.value = list }

	fun addBytes(n: Long) { _bytes.update { it + n } }

	fun setTail(jobId: String, lines: List<String>) {
		_logs.update { it + (jobId to lines) }
	}

	fun apply(event: AgentEvent) {
		when (event.type) {
			"job" -> event.job?.let { incoming ->
				_jobs.update { current ->
					val idx = current.indexOfFirst { it.id == incoming.id }
					if (idx == -1) listOf(incoming) + current
					else current.toMutableList().also { it[idx] = incoming }
				}
			}

			"log" -> {
				val id = event.jobId ?: return
				val line = event.line ?: return
				_logs.update { map ->
					val existing = map[id].orEmpty()
					// Bounded: a chatty job should not grow without limit on a phone.
					val next = (existing + line).takeLast(MAX_LINES)
					map + (id to next)
				}
			}
		}
	}

	fun reset() {
		_jobs.value = emptyList()
		_logs.value = emptyMap()
		_bytes.value = 0L
		_connected.value = false
	}

	private const val MAX_LINES = 2000
}
