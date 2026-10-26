{{- define "slopify.apiAuthEnv" -}}
- name: OIDC_ISSUER
  valueFrom:
    secretKeyRef:
      name: oidc-client-slopify-owner-secrets
      key: OIDC_IDP_URI
- name: OIDC_CLIENT_ORIGIN
  valueFrom:
    secretKeyRef:
      name: oidc-client-slopify-owner-secrets
      key: OIDC_CLIENT_ORIGIN
- name: OIDC_AUDIENCE
  value: "$(OIDC_CLIENT_ORIGIN)/api"
{{- end -}}
