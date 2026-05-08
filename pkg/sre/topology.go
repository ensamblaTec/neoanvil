package sre

// Identidad Topológica Fuerte (BGP Hijacking Protection)
// ED25519 PubKeys hardcodeadas para nodos seminales del Swarm SRE.
var BootstrapNodes = map[string]string{
	"10.0.0.10": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // Factory Celaya
	"10.0.0.11": "a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3", // Datacenter Berlin
	"10.0.0.12": "2c9438080f5d0ecfc4d70b435e2363b1fb2fa373b573e6da8a67eb3f1fde1885", // Facility Tokyo
}

// VerifyRoutingIdentity aborta la comunicación PQC si el BGP nos rutea hacia un impostor
// clonando el IP Address sin contar con el Hardware Cryptográfico firmado original.
func VerifyRoutingIdentity(ip string, presentedPubKey string) bool {
	expected, ok := BootstrapNodes[ip]
	if !ok {
		return true // Nodos hoja dinámicos (No seminales) pasan la barrera para PQC Handshake.
	}
	return expected == presentedPubKey
}
