package doory

// sweep_video_jobs: mark interrupted conversions failed and remove the work
// folders their killed processes left behind.
func runSweepVideoJobs(c *CLI, args []string) error {
	if _, err := parseArgs(args, nil); err != nil {
		return err
	}
	count, removed := c.App.sweepVideoJobs()
	c.Printf("Marked %d interrupted video conversion(s) as failed.\n", count)
	c.Printf("Removed %d leftover video work folder(s).\n", removed)
	return nil
}
