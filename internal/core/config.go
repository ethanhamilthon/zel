package core

import "zel/internal/config"

type Settings = config.Settings
type Provider = config.Provider
type UI = config.UI
type Hook = config.Hook
type SystemBlock = config.SystemBlock

const ShellTimeout = config.ShellTimeout
const ShellMaxTimeout = config.ShellMaxTimeout
const ShellOutput = config.ShellOutput
const HookTimeout = config.HookTimeout
const APIResponses = config.APIResponses
const APIChat = config.APIChat

var Efforts = config.Efforts
var AccentColors = config.AccentColors
var DefaultSettings = config.DefaultSettings
var Redact = config.Redact
var FlagConfig = config.FlagConfig
var ConfigPaths = config.ConfigPaths
var ZelHome = config.ZelHome
var ResolvePath = config.ResolvePath
var DisplayPath = config.DisplayPath
var DBPath = config.DBPath

var InitProject = config.InitProject
