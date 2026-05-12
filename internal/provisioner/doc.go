// Package provisioner places worker agents on hosts.
//
// The Provisioner interface is shaped for both same-host placement
// (LocalProvisioner) and remote placement (SSHProvisioner today,
// RailwayProvisioner in a follow-up). Provision returns an Agent that
// already carries a Transport ready to start `claude` subprocesses, plus
// any tailnet hostname the worker is reachable at.
package provisioner
