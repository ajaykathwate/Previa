// Package appoutput shows an owner what a running preview's app prints, live.
//
// Viewing the page starts a follower on a worker (FollowWorker), which reads
// the app container's output through Docker, redacts it like the startup log
// (the container's LabelRedactEnv) and stores at most the newest 1,000 lines.
// The page streams them as they arrive. When nobody has watched for a
// minute the follower stops and deletes the lines, so nothing is kept.
package appoutput
