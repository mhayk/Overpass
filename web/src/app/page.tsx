import Workspace from '@/components/Workspace';

/**
 * A server component that renders one client island.
 *
 * The page itself has no interactivity and no browser dependency, so it stays
 * on the server. Only Workspace and the globe below it cross into the client
 * bundle, which is what keeps Cesium off any route that does not show it.
 */
export default function Home(): React.JSX.Element {
  return <Workspace />;
}
