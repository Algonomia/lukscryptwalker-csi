package secrets

import "k8s.io/klog"

// ExtractSecretParams extracts secret parameters from StorageClass parameters and volume context
func ExtractSecretParams(scParams, volumeContext map[string]string) SecretParams {
	params := SecretParams{
		PassphraseKey: DefaultPassphraseKey,
	}

	// Extract LUKS secret reference
	// First check volumeContext (from CSI request), then fall back to scParams
	if name, ok := volumeContext[SecretNameKey]; ok {
		params.LUKSSecret.Name = name
	} else if scParams != nil {
		if name, ok := scParams[SecretNameKey]; ok {
			params.LUKSSecret.Name = name
		}
	}

	if ns, ok := volumeContext[SecretNamespaceKey]; ok {
		params.LUKSSecret.Namespace = ns
	} else if scParams != nil {
		if ns, ok := scParams[SecretNamespaceKey]; ok {
			params.LUKSSecret.Namespace = ns
		}
	}

	// Extract passphrase key
	if key, ok := volumeContext[PassphraseKeyParam]; ok {
		params.PassphraseKey = key
	} else if scParams != nil {
		if key, ok := scParams[PassphraseKeyParam]; ok {
			params.PassphraseKey = key
		}
	}

	// Extract S3 secret reference (all S3 config is in a single secret)
	if name, ok := volumeContext[S3SecretName]; ok {
		params.S3Secret.Name = name
	} else if scParams != nil {
		if name, ok := scParams[S3SecretName]; ok {
			params.S3Secret.Name = name
		}
	}

	if ns, ok := volumeContext[S3SecretNamespace]; ok {
		params.S3Secret.Namespace = ns
	} else if scParams != nil {
		if ns, ok := scParams[S3SecretNamespace]; ok {
			params.S3Secret.Namespace = ns
		}
	}

	klog.V(4).Infof("Extracted secret params: LUKS=%s/%s, S3=%s/%s, passphraseKey=%s",
		params.LUKSSecret.Namespace, params.LUKSSecret.Name,
		params.S3Secret.Namespace, params.S3Secret.Name,
		params.PassphraseKey)

	return params
}

// HasLUKSSecret returns true if LUKS secret reference is complete
func (p SecretParams) HasLUKSSecret() bool {
	return p.LUKSSecret.Name != "" && p.LUKSSecret.Namespace != ""
}

// HasS3Secret returns true if S3 secret reference is complete
func (p SecretParams) HasS3Secret() bool {
	return p.S3Secret.Name != "" && p.S3Secret.Namespace != ""
}