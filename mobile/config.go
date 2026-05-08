//go:build mobile

package mobile

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// MobileConfig is the gomobile-bound subset of neo.yaml that the Android
// side cares about. We deliberately do NOT expose the full
// pkg/config.NeoConfig — gomobile bind cannot handle pointer-heavy
// structs with channels, time.Duration, or interface fields. Keeping
// this struct flat + string/int makes the JNI surface small and the
// generated AAR header readable on the Kotlin side.
type MobileConfig struct {
	BucketURL        string   `yaml:"bucket_url"`
	BucketAccessKey  string   `yaml:"bucket_access_key"`
	BucketSecretKey  string   `yaml:"bucket_secret_key"`
	PassphraseHint   string   `yaml:"passphrase_hint"`
	Peers            []string `yaml:"peers"`
	TailscaleAuthKey string   `yaml:"tailscale_auth_key"`
	PowerPolicy      string   `yaml:"power_policy"` // always | wifi | charging | manual
	NodeID           string   `yaml:"node_id"`      // optional override; if set, used as `provided` for BrainNodeID
	NodeFingerprint  string   `yaml:"node_fingerprint"`
}

// LoadConfigBytes parses a YAML byte slice into a MobileConfig. Used
// when the Android Kotlin layer reads the YAML from app-private storage
// or assets and passes the bytes verbatim through JNI.
//
// We do NOT use pkg/config.LoadConfig because:
//   - LoadConfig walks the directory tree looking for neo.yaml; that
//     walk depends on /etc and /home semantics that don't apply on
//     Android.
//   - LoadConfig returns the full pkg/config.NeoConfig (~50 nested
//     structs), most of which are irrelevant on a mobile peer. The
//     surface area would be enormous and gomobile-incompatible.
//   - LoadConfig has a write-back path (backfill) that re-saves the
//     parsed YAML if defaults were filled in. That's hostile on
//     Android where the source bytes are typically immutable assets.
//
// Returns an error only on parse failure. Empty fields stay empty —
// the caller (Kotlin) is responsible for surfacing missing required
// values to the user.
func LoadConfigBytes(data []byte) (*MobileConfig, error) {
	if len(data) == 0 {
		return nil, errors.New("LoadConfigBytes: empty input")
	}
	var cfg MobileConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ConfigToYAMLBytes produces a YAML serialization of cfg. Used when the
// Android UI lets the user edit fields in a Compose form and we need to
// persist the result back to app-private storage.
//
// Unlike LoadConfigBytes there's no validation; the caller is trusted
// to have sanitised the inputs. We return errors only for marshal
// failures (extremely unlikely with the flat MobileConfig shape).
func ConfigToYAMLBytes(cfg *MobileConfig) ([]byte, error) {
	if cfg == nil {
		return nil, errors.New("ConfigToYAMLBytes: nil config")
	}
	return yaml.Marshal(cfg)
}
