package api;
public abstract class Plugin {
    public void onEnable() {}
    public abstract void onCommand(String label, int argc);
    protected void log(String m) { System.out.println("[log] " + m); }
    public final void start() { onEnable(); onCommand("go", 3); }
}
