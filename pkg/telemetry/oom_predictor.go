package telemetry

import (
	"fmt"
)

func PredictASTMemory(fileSizeBytes int) error {

	estimatedRAM := int64(fileSizeBytes) * 50

	const maxSafeRAM = 500 * 1024 * 1024

	if estimatedRAM > maxSafeRAM {
		return fmt.Errorf("[SRE-OOM-PREVENTED] El archivo es demasiado grande (%d bytes). El AST requeriría aproximadamente %d MB de RAM, superando el límite seguro de 500 MB. Operación vetada para proteger el servidor MCP. Usa 'neo_run_command' con sed/awk si necesitas editar este archivo gigante.", fileSizeBytes, estimatedRAM/1024/1024)
	}

	return nil
}
