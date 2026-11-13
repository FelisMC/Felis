package best.lolicon.felis.link;

/** The player and operator console addresses; null where no host is known. */
public record ConsoleUrls(String panel, String admin) {
    /** orElse fills each missing address from the fallback. */
    public ConsoleUrls orElse(ConsoleUrls fallback) {
        return new ConsoleUrls(panel != null ? panel : fallback.panel(), admin != null ? admin : fallback.admin());
    }
}
