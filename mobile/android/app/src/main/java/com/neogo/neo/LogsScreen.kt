package com.neogo.neo

// PILAR XXVI 137.B.2 — Logs tab: surfaces in-memory service log ring.
//
// Awaiting Android Studio compile validation.

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.neogo.neo.service.NeoMeshService

@Composable
fun LogsScreen(modifier: Modifier = Modifier) {
    val logs = NeoMeshService.logRing.toList()
    Column(modifier = modifier.fillMaxSize().padding(16.dp)) {
        Text("Logs", style = MaterialTheme.typography.headlineMedium)
        LazyColumn {
            items(logs.asReversed()) { line ->
                Text(
                    text = line,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.padding(vertical = 2.dp),
                )
            }
        }
    }
}
